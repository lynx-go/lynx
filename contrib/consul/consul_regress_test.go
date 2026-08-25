package consul

// 本文件是 review-2026-08-25 RC 系列修复的回归测试：
// RC-01（Register ctx）、RC-03（Node.Address 回落）、RC-05（index 回绕）、
// RC-09（Watch/Close 竞态）、RC-17（lynx_endpoints 解码告警）、
// RC-18（零权重规格化）、RC-23（Registrar×Consul 组合链路）。

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/lynx-go/lynx/eventbus"
)

// fakeAppContext 是 Registrar.Init 需要的最小 lynx.AppContext。
type fakeAppContext struct{ ctx context.Context }

func (f *fakeAppContext) Context() context.Context       { return f.ctx }
func (f *fakeAppContext) Config() lynx.Config            { return nil }
func (f *fakeAppContext) Logger(...any) *slog.Logger     { return slog.Default() }
func (f *fakeAppContext) Bus() eventbus.Bus              { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (f *fakeAppContext) HealthCheckers() []lynx.Checker { return nil }
func (f *fakeAppContext) Close()                         {}

// eventually 在 deadline 内反复调用 cond 直到为真。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// hungAgentFallback 是挂死 handler 的兜底出口。测试断言的是客户端侧
// 按 ctx 预算返回；而带请求体的请求取消后服务端可能感知不到断开
// （Go http 在部分平台上不立即关闭连接），httptest.Server.Close 会等
// 全部 outstanding handler 返回——没有兜底会让测试清理永久阻塞。
// 6s 远大于被断言的 3s 客户端预算，不影响「agent 挂死」的语义。
const hungAgentFallback = 6 * time.Second

// newHungAgentServer 返回注册请求永不响应的假 agent（挂到客户端断开
// 或兜底超时为止），用于验证 Register 的 ctx 传递（RC-01）。hits 统计
// 到达的注册尝试次数。
func newHungAgentServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	hang := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(hungAgentFallback):
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/agent/service/register", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hang(w, r)
	})
	mux.HandleFunc("/", hang)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newHungClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	cfg := api.DefaultConfig()
	cfg.Address = strings.TrimPrefix(srv.URL, "http://")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestRegisterRespectsContext 锁定 RC-01：Register 必须把 ctx 绑到
// consul api 请求上——挂死 agent 时按 ctx 预算返回错误，而非无限等待。
func TestRegisterRespectsContext(t *testing.T) {
	srv, _ := newHungAgentServer(t)
	c := newHungClient(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Register(ctx, twoEndpointInstance()); err == nil {
		t.Fatal("hung agent must fail Register")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Register must honor ctx deadline, took %s", elapsed)
	}
}

// TestRegistrarBudgetAgainstHungAgent 锁定 RC-01 的端到端效果：Registrar
// 侧 3s 注册预算对 Consul 后端真实生效——fail_fast 时 Start 在预算内
// 返回错误而非挂死。
func TestRegistrarBudgetAgainstHungAgent(t *testing.T) {
	srv, _ := newHungAgentServer(t)
	c := newHungClient(t, srv)

	r := registry.NewRegistrar(c,
		registry.WithServiceName("svc"),
		registry.WithInstanceID("i1"),
		registry.WithStaticEndpoints(registry.Endpoint{
			Protocol: registry.ProtocolHTTP, Address: "10.0.0.1:8080",
		}),
	)
	if err := r.Init(&fakeAppContext{ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- r.Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fail_fast must surface the register error")
		}
		if elapsed := time.Since(start); elapsed > 4500*time.Millisecond {
			t.Fatalf("Start must return within the 3s budget, took %s", elapsed)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Start hung against dead agent: Register ctx not propagated")
	}
}

// TestRegistrarRetryLoopNotHungAgainstHungAgent 锁定 RC-01 的非 fail_fast
// 路径：重试 goroutine 每轮受 3s 预算约束正常返回并继续重试，不再永久
// 挂起（挂死 agent 会持续收到注册尝试）。
func TestRegistrarRetryLoopNotHungAgainstHungAgent(t *testing.T) {
	srv, hits := newHungAgentServer(t)
	c := newHungClient(t, srv)

	r := registry.NewRegistrar(c,
		registry.WithServiceName("svc"),
		registry.WithInstanceID("i1"),
		registry.WithFailFast(false),
		registry.WithStaticEndpoints(registry.Endpoint{
			Protocol: registry.ProtocolHTTP, Address: "10.0.0.1:8080",
		}),
	)
	if err := r.Init(&fakeAppContext{ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	defer func() {
		_ = r.Stop(context.Background())
		<-done
	}()

	// 第一次尝试 ~3s 超时 + 1s 退避 + 第二次尝试：两轮都按预算返回。
	eventually(t, "retry loop keeps attempting within budget", func() bool {
		return hits.Load() >= 2
	})
}

// TestGetServiceFallsBackToNodeAddress 锁定 RC-03：服务未带
// Service.Address 注册时回落 Node.Address 组地址；两者皆空跳过 + Warn，
// 绝不产出裸 ":port" Endpoint。
func TestGetServiceFallsBackToNodeAddress(t *testing.T) {
	t.Run("service address empty falls back to node", func(t *testing.T) {
		f, srv := newFakeConsul(t)
		f.stripServiceAddress = true
		f.nodeAddress = "10.9.9.9"
		c := newTestClient(t, srv)

		if err := c.Register(context.Background(), twoEndpointInstance()); err != nil {
			t.Fatal(err)
		}
		got, err := c.GetService(context.Background(), "svc", registry.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 instance, got %d", len(got))
		}
		if got[0].Endpoints[0].Address != "10.9.9.9:8080" {
			t.Fatalf("must fall back to node address, got %q", got[0].Endpoints[0].Address)
		}
	})
	t.Run("no address at all is skipped with warn", func(t *testing.T) {
		f, srv := newFakeConsul(t)
		var buf bytes.Buffer
		c := newTestClient(t, srv, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

		// 直接塞一条 Service.Address 与 Node.Address 皆空的目录项。
		f.mu.Lock()
		f.services["svc"] = []*api.ServiceEntry{{
			Node: &api.Node{Node: "n1", Address: ""},
			Service: &api.AgentService{ID: "noaddr", Service: "svc", Address: "", Port: 8080,
				Meta: map[string]string{metaMainProtocolKey: "http"}},
			Checks: api.HealthChecks{{Status: api.HealthPassing}},
		}}
		f.bump()
		f.mu.Unlock()

		got, err := c.GetService(context.Background(), "svc", registry.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("entry without any address must be skipped, got %+v", got)
		}
		if !strings.Contains(buf.String(), "no dialable address") {
			t.Fatalf("skip must be warned, log: %q", buf.String())
		}
	})
}

// TestWatchIndexRewindRecovers 锁定 RC-05：Raft index 回绕（leader 变更/
// 快照恢复）后，watcher 按官方模式把 WaitIndex 重置为 0 立即重查并恢复
// 推送能力。
func TestWatchIndexRewindRecovers(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv)
	ctx := context.Background()

	w, err := c.Watch(ctx, "svc", registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	if _, err := w.Next(); err != nil { // 首快照（空）
		t.Fatal(err)
	}
	if err := c.Register(ctx, twoEndpointInstance()); err != nil {
		t.Fatal(err)
	}
	snap, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 1 {
		t.Fatalf("want 1 instance after register, got %d", len(snap))
	}
	// 第二个实例：把 watcher 的 WaitIndex 推到 2，为回绕制造落差。
	inst2 := twoEndpointInstance()
	inst2.ID = "i2"
	inst2.Endpoints = []registry.Endpoint{
		{Protocol: registry.ProtocolHTTP, Address: "10.0.0.2:8080"},
	}
	if err := c.Register(ctx, inst2); err != nil {
		t.Fatal(err)
	}
	if snap, err = w.Next(); err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("want 2 instances, got %d", len(snap))
	}

	// 回绕前记录查询水位，随后把 index 重置回低值（WaitIndex 仍是 2）。
	before := len(f.queriesSnapshot())
	f.rewindIndex(1)

	// 恢复证明 1（官方模式）：回绕后出现一次不带 index 参数（WaitIndex=0）
	// 的重查。修复前 watcher 会带着过期 WaitIndex 继续阻塞。
	eventually(t, "WaitIndex reset to 0 after rewind", func() bool {
		for _, q := range f.queriesSnapshot()[before:] {
			if !strings.Contains(q, "index=") {
				return true
			}
		}
		return false
	})

	// 恢复证明 2：回绕后的目录变更仍能推送到 Next。
	inst3 := twoEndpointInstance()
	inst3.ID = "i3"
	inst3.Endpoints = []registry.Endpoint{
		{Protocol: registry.ProtocolHTTP, Address: "10.0.0.3:8080"},
	}
	if err := c.Register(ctx, inst3); err != nil {
		t.Fatal(err)
	}
	nextDone := make(chan []registry.Instance, 1)
	go func() {
		s, err := w.Next()
		if err != nil {
			t.Error(err)
			return
		}
		nextDone <- s
	}()
	select {
	case s := <-nextDone:
		if len(s) != 2 {
			t.Fatalf("watch must recover after index rewind, got %+v", idsOf(s))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch lost push ability after index rewind")
	}
}

func idsOf(instances []registry.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, inst := range instances {
		out = append(out, inst.ID)
	}
	return out
}

// TestWatchCloseRaceNoWatcherLeak 锁定 RC-09：closed 检查并入注册临界区
// 后，Close 与并发 Watch 的任何交错都不会留下「无人停」的 watcher。
func TestWatchCloseRaceNoWatcherLeak(t *testing.T) {
	_, srv := newFakeConsul(t)
	for i := 0; i < 60; i++ {
		cfg := api.DefaultConfig()
		cfg.Address = strings.TrimPrefix(srv.URL, "http://")
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}

		const n = 4
		var wg sync.WaitGroup
		for j := 0; j < n; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w, werr := c.Watch(context.Background(), "svc", registry.Filter{})
				if werr == nil {
					// 成功建立的 watcher 由 Close 负责停；此处只需排空。
					go func() { _, _ = w.Next() }()
				}
			}()
		}
		runtime.Gosched() // 提高 Close 落在 Watch 注册窗口内的概率
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()

		c.mu.Lock()
		leaked := len(c.watchers)
		c.mu.Unlock()
		if leaked != 0 {
			t.Fatalf("round %d: %d watchers leaked by Watch/Close race", i, leaked)
		}
	}
}

// TestDecodeEndpointsFailureWarns 锁定 RC-17：lynx_endpoints 解码失败记
// Warn，主 Endpoint 仍可用，不再静默退化。
func TestDecodeEndpointsFailureWarns(t *testing.T) {
	f, srv := newFakeConsul(t)
	var buf bytes.Buffer
	c := newTestClient(t, srv, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

	f.mu.Lock()
	f.services["svc"] = []*api.ServiceEntry{{
		Node: &api.Node{Node: "n1", Address: "10.0.0.9"},
		Service: &api.AgentService{ID: "broken", Service: "svc", Address: "10.0.0.9", Port: 8080,
			Meta: map[string]string{
				metaMainProtocolKey: "http",
				metaEndpointsKey:    "{not-json",
			}},
		Checks: api.HealthChecks{{Status: api.HealthPassing}},
	}}
	f.bump()
	f.mu.Unlock()

	got, err := c.GetService(context.Background(), "svc", registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Endpoints) != 1 {
		t.Fatalf("main endpoint must survive decode failure, got %+v", got)
	}
	if !strings.Contains(buf.String(), "lynx_endpoints decode failed") {
		t.Fatalf("decode failure must be warned, log: %q", buf.String())
	}
}

// TestRegisterZeroWeightNormalized 锁定 RC-18：Weight 零值规格化为
// Weights.Passing=1（Consul 语义 Passing=0 表示不可用）；非零值原样透传。
func TestRegisterZeroWeightNormalized(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv)

	inst := twoEndpointInstance()
	inst.Weight = 0
	if err := c.Register(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	reg := f.lastRegistration()
	if reg.Weights == nil || reg.Weights.Passing != 1 {
		t.Fatalf("zero weight must normalize Passing to 1, got %+v", reg.Weights)
	}

	inst.Weight = 100
	if err := c.Register(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	if reg := f.lastRegistration(); reg.Weights == nil || reg.Weights.Passing != 100 {
		t.Fatalf("non-zero weight must pass through, got %+v", reg.Weights)
	}
}

// TestRegistrarConsulHeartbeatCombo 锁定 RC-23 的组合链路：Registrar ×
// Consul TTL 后端——注册落目录、Heartbeat 周期性 UpdateTTL、Stop 注销。
func TestRegistrarConsulHeartbeatCombo(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv, WithCheckType(CheckTypeTTL))

	r := registry.NewRegistrar(c,
		registry.WithServiceName("svc"),
		registry.WithInstanceID("i1"),
		registry.WithHeartbeatInterval(40*time.Millisecond),
		registry.WithStaticEndpoints(registry.Endpoint{
			Protocol: registry.ProtocolHTTP, Address: "10.0.0.1:8080",
		}),
	)
	if err := r.Init(&fakeAppContext{ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	defer func() {
		_ = r.Stop(context.Background())
		<-done
	}()

	// 注册成功 + 至少两次 TTL 心跳到达 Consul。
	eventually(t, "ttl heartbeats delivered", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		count := 0
		for _, id := range f.ttlUpdates {
			if id == "service:i1" {
				count++
			}
		}
		return count >= 2
	})

	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	deregistered := append([]string(nil), f.deregistered...)
	f.mu.Unlock()
	found := false
	for _, id := range deregistered {
		if id == "i1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Stop must deregister from consul, got %v", deregistered)
	}
}
