package registry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lynx-go/lynx/eventbus"
	"time"

	"github.com/lynx-go/lynx"
)

// fakeAppContext 是 Init 测试用的最小 AppContext。
type fakeAppContext struct {
	ctx      context.Context
	checkers []lynx.Checker
}

func (f *fakeAppContext) Context() context.Context       { return f.ctx }
func (f *fakeAppContext) Config() lynx.Config            { return nil }
func (f *fakeAppContext) Bus() eventbus.Bus              { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (f *fakeAppContext) Logger(...any) *slog.Logger     { return slog.Default() }
func (f *fakeAppContext) HealthCheckers() []lynx.Checker { return f.checkers }
func (f *fakeAppContext) Close()                         {}

func newFakeAppContext() *fakeAppContext {
	return &fakeAppContext{ctx: context.Background()}
}

// fakeRegistry 记录调用并可注入失败。
type fakeRegistry struct {
	mu              sync.Mutex
	registerFails   int // 前 N 次 Register 返回错误
	heartbeatErr    error
	instances       []Instance
	heartbeatCalls  int
	deregisterCalls int
	closeCalls      int
}

var errFakeBackend = errors.New("fake backend error")

func (f *fakeRegistry) Register(_ context.Context, inst Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerFails > 0 {
		f.registerFails--
		return errFakeBackend
	}
	f.instances = append(f.instances, inst)
	return nil
}

func (f *fakeRegistry) Deregister(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregisterCalls++
	f.instances = nil
	return nil
}

func (f *fakeRegistry) Heartbeat(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeatCalls++
	return f.heartbeatErr
}

func (f *fakeRegistry) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func (f *fakeRegistry) registeredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.instances)
}

func (f *fakeRegistry) heartbeats() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeatCalls
}

func mustInit(t *testing.T, r *Registrar, actx *fakeAppContext) {
	t.Helper()
	if err := r.Init(actx); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

func TestRegistrarInitNameValidation(t *testing.T) {
	valid := []string{"user-service", "a", "A0", strings.Repeat("a", 63), "a-b-c"}
	for _, name := range valid {
		r := NewRegistrar(NewMemory(), WithServiceName(name))
		if err := r.Init(newFakeAppContext()); err != nil {
			t.Fatalf("name %q should be valid: %v", name, err)
		}
	}
	invalid := []string{"", "-bad", "bad-", "has space", "under_score", strings.Repeat("a", 64), "中文"}
	for _, name := range invalid {
		r := NewRegistrar(NewMemory(), WithServiceName(name))
		err := r.Init(newFakeAppContext())
		if !errors.Is(err, ErrBadName) {
			t.Fatalf("name %q: want ErrBadName, got %v", name, err)
		}
	}
	// 未设置 service_name 且 Meta 为空 → 空名校验失败。
	r := NewRegistrar(NewMemory())
	if err := r.Init(newFakeAppContext()); !errors.Is(err, ErrBadName) {
		t.Fatalf("empty meta name: want ErrBadName, got %v", err)
	}
}

func TestRegistrarInitAdvertiseHost(t *testing.T) {
	bare := WithStaticEndpoints(Endpoint{Protocol: ProtocolHTTP, Address: ":8080"})

	t.Run("env ipv6 joinhostport", func(t *testing.T) {
		t.Setenv(advertiseHostEnv, "2001:db8::1")
		r := NewRegistrar(NewMemory(), WithServiceName("svc"), bare)
		mustInit(t, r, newFakeAppContext())
		if got := r.static[0].Address; got != "[2001:db8::1]:8080" {
			t.Fatalf("want [2001:db8::1]:8080, got %q", got)
		}
	})

	t.Run("option beats env", func(t *testing.T) {
		t.Setenv(advertiseHostEnv, "10.9.9.9")
		r := NewRegistrar(NewMemory(), WithServiceName("svc"), bare, WithAdvertiseHost("10.0.0.1"))
		mustInit(t, r, newFakeAppContext())
		if got := r.static[0].Address; got != "10.0.0.1:8080" {
			t.Fatalf("want 10.0.0.1:8080, got %q", got)
		}
	})

	t.Run("missing host fails", func(t *testing.T) {
		r := NewRegistrar(NewMemory(), WithServiceName("svc"), bare)
		if err := r.Init(newFakeAppContext()); err == nil {
			t.Fatal("bare :port without advertise host must fail Init")
		}
	})

	t.Run("full hostport skips host", func(t *testing.T) {
		r := NewRegistrar(NewMemory(), WithServiceName("svc"),
			WithStaticEndpoints(Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}))
		mustInit(t, r, newFakeAppContext())
	})

	t.Run("no endpoints skips host", func(t *testing.T) {
		r := NewRegistrar(NewMemory(), WithServiceName("svc"))
		mustInit(t, r, newFakeAppContext())
	})

	t.Run("malformed address fails", func(t *testing.T) {
		r := NewRegistrar(NewMemory(), WithServiceName("svc"),
			WithStaticEndpoints(Endpoint{Protocol: ProtocolHTTP, Address: "no-port"}),
			WithAdvertiseHost("10.0.0.1"))
		if err := r.Init(newFakeAppContext()); err == nil {
			t.Fatal("malformed address must fail Init")
		}
	})
}

func TestRegistrarFailFast(t *testing.T) {
	backend := &fakeRegistry{registerFails: 100}
	r := NewRegistrar(backend, WithServiceName("svc"))
	mustInit(t, r, newFakeAppContext())

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fail_fast=true: Start must return the register error")
		}
	case <-time.After(time.Second):
		t.Fatal("fail_fast=true: Start must not block after register failure")
	}
}

func TestRegistrarFailSafeRetriesInBackground(t *testing.T) {
	backend := &fakeRegistry{registerFails: 1} // 首次失败，重试成功
	r := NewRegistrar(backend, WithServiceName("svc"), WithFailFast(false))
	mustInit(t, r, newFakeAppContext())

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()

	// Start 必须保持阻塞（oklog/run 里返回会拆掉整个 group）。
	select {
	case err := <-done:
		t.Fatalf("fail_fast=false: Start returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// 注册成功前 CheckHealth 为 ErrNotRegistered。
	if err := r.CheckHealth(); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("pre-register: want ErrNotRegistered, got %v", err)
	}

	// 后台重试（退避 1s）成功后注册成功且 CheckHealth 变 nil。
	waitFor(t, 3*time.Second, func() bool { return backend.registeredCount() == 1 })
	waitFor(t, time.Second, func() bool { return r.CheckHealth() == nil })

	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after Stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start must unblock after Stop")
	}
}

func TestRegistrarHeartbeatFailures(t *testing.T) {
	backend := &fakeRegistry{heartbeatErr: errFakeBackend}
	r := NewRegistrar(backend, WithServiceName("svc"), WithHeartbeatInterval(20*time.Millisecond))
	mustInit(t, r, newFakeAppContext())

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	defer func() {
		_ = r.Stop(context.Background())
		<-done
	}()

	waitFor(t, time.Second, func() bool { return r.CheckHealth() == nil })

	// 连续 3 次心跳失败 → ErrHeartbeatFailed。
	waitFor(t, 2*time.Second, func() bool {
		return backend.heartbeats() >= heartbeatFailLimit && errors.Is(r.CheckHealth(), ErrHeartbeatFailed)
	})
}

func TestRegistrarStopBeforeStart(t *testing.T) {
	backend := &fakeRegistry{}
	r := NewRegistrar(backend, WithServiceName("svc"))
	mustInit(t, r, newFakeAppContext())

	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := r.CheckHealth(); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("after Stop: want ErrNotRegistered, got %v", err)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("Registry.Close must be called exactly once per Stop path... got %d", backend.closeCalls)
	}
	// Stop 之后 Start 立即返回。
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
}

func TestRegistrarDeregisterHook(t *testing.T) {
	mem := NewMemory()
	r := NewRegistrar(mem, WithServiceName("svc"), WithInstanceID("i1"),
		WithStaticEndpoints(Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}))
	mustInit(t, r, newFakeAppContext())

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	defer func() {
		_ = r.Stop(context.Background())
		<-done
	}()
	waitFor(t, time.Second, func() bool { return r.CheckHealth() == nil })

	hook := r.DeregisterHook()
	if err := hook(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mem.GetService(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("hook must deregister, got %v", ids(got))
	}
	if err := r.CheckHealth(); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("after hook: want ErrNotRegistered, got %v", err)
	}
	// 幂等：再次调用 no-op。
	if err := hook(context.Background()); err != nil {
		t.Fatalf("second hook: %v", err)
	}
}

// drainFlag 是可手动置位的排水 checker，模拟框架 drainChecker。
type drainFlag struct{ on atomic.Bool }

func (d *drainFlag) CheckHealth() error {
	if d.on.Load() {
		return lynx.ErrDraining
	}
	return nil
}

func TestRegistrarWatchDrain(t *testing.T) {
	mem := NewMemory()
	drain := &drainFlag{}
	actx := newFakeAppContext()
	actx.checkers = []lynx.Checker{drain}

	r := NewRegistrar(mem, WithServiceName("svc"), WithInstanceID("i1"))
	mustInit(t, r, actx)

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	defer func() {
		_ = r.Stop(context.Background())
		<-done
	}()
	waitFor(t, time.Second, func() bool { return r.CheckHealth() == nil })

	// 排水置位 → watchDrain 安全网自动注销（用户忘了 Bind 的场景）。
	drain.on.Store(true)
	waitFor(t, 2*time.Second, func() bool {
		got, _ := mem.GetService(context.Background(), "svc", Filter{})
		return len(got) == 0
	})
	if err := r.CheckHealth(); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("after drain deregister: want ErrNotRegistered, got %v", err)
	}
}

func TestRegistrarAffectReadinessFalse(t *testing.T) {
	r := NewRegistrar(&fakeRegistry{}, WithServiceName("svc"), WithAffectReadiness(false))
	mustInit(t, r, newFakeAppContext())
	// 未注册也恒 nil：不参与 readiness 聚合的实际效果。
	if err := r.CheckHealth(); err != nil {
		t.Fatalf("affect_readiness=false: want nil, got %v", err)
	}
}

// lateAdvertiser 在就绪前返回 nil。
type lateAdvertiser struct {
	delay atomic.Int64 // 剩余返回 nil 的次数
	ep    Endpoint
}

func (a *lateAdvertiser) Endpoints() []Endpoint {
	if a.delay.Add(-1) >= 0 {
		return nil
	}
	return []Endpoint{a.ep}
}

func TestRegistrarWaitsForAdvertiser(t *testing.T) {
	mem := NewMemory()
	adv := &lateAdvertiser{ep: Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}}
	adv.delay.Store(2) // 前两次轮询未就绪
	r := NewRegistrar(mem, WithServiceName("svc"), WithInstanceID("i1"),
		WithAdvertisers(adv), WithAdvertiseTimeout(2*time.Second))
	mustInit(t, r, newFakeAppContext())

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	defer func() {
		_ = r.Stop(context.Background())
		<-done
	}()

	waitFor(t, 2*time.Second, func() bool { return r.CheckHealth() == nil })
	got, err := mem.GetService(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Endpoints) != 1 || got[0].Endpoints[0].Address != "10.0.0.1:8080" {
		t.Fatalf("advertised endpoint not registered: %+v", got)
	}
}

func TestRegistrarAdvertiseTimeout(t *testing.T) {
	never := Static(ProtocolHTTP, "") // 永远返回 nil
	r := NewRegistrar(NewMemory(), WithServiceName("svc"),
		WithAdvertisers(never), WithAdvertiseTimeout(150*time.Millisecond))
	mustInit(t, r, newFakeAppContext())

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("advertise timeout without static endpoints must fail Start")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start must return after advertise_timeout")
	}
}

// barePortAdvertiser 恒定返回裸端口 Endpoint（模拟 AdvertiseAddr 未设置且
// 监听器只报 ":8080" 的场景）。
type barePortAdvertiser struct{ ep Endpoint }

func (a *barePortAdvertiser) Endpoints() []Endpoint { return []Endpoint{a.ep} }

// TestRegistrarSkipsBarePortWithoutAdvertiseHost 锁定 RC-02：instance() 对
// 无法补全的裸端口 Advertiser Endpoint 跳过 + Warn，不再让裸 ":8080"
// 入目录（违反 Endpoint host:port 契约，registry.go）。
func TestRegistrarSkipsBarePortWithoutAdvertiseHost(t *testing.T) {
	t.Run("skip when advertise host missing", func(t *testing.T) {
		backend := &fakeRegistry{}
		r := NewRegistrar(backend, WithServiceName("svc"),
			WithStaticEndpoints(Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}),
			WithAdvertisers(&barePortAdvertiser{ep: Endpoint{Protocol: ProtocolGRPC, Address: ":9090"}}),
		)
		mustInit(t, r, newFakeAppContext())

		done := make(chan error, 1)
		go func() { done <- r.Start(context.Background()) }()
		defer func() {
			_ = r.Stop(context.Background())
			<-done
		}()
		waitFor(t, time.Second, func() bool { return r.CheckHealth() == nil })

		backend.mu.Lock()
		var got []Endpoint
		if len(backend.instances) > 0 {
			got = backend.instances[0].Endpoints
		}
		backend.mu.Unlock()
		if len(got) != 1 || got[0].Address != "10.0.0.1:8080" {
			t.Fatalf("bare :9090 must be skipped, registered endpoints: %+v", got)
		}
	})
	t.Run("completed when advertise host present", func(t *testing.T) {
		backend := &fakeRegistry{}
		r := NewRegistrar(backend, WithServiceName("svc"),
			WithAdvertiseHost("10.1.1.1"),
			WithAdvertisers(&barePortAdvertiser{ep: Endpoint{Protocol: ProtocolGRPC, Address: ":9090"}}),
		)
		mustInit(t, r, newFakeAppContext())

		done := make(chan error, 1)
		go func() { done <- r.Start(context.Background()) }()
		defer func() {
			_ = r.Stop(context.Background())
			<-done
		}()
		waitFor(t, time.Second, func() bool { return r.CheckHealth() == nil })

		backend.mu.Lock()
		var got []Endpoint
		if len(backend.instances) > 0 {
			got = backend.instances[0].Endpoints
		}
		backend.mu.Unlock()
		if len(got) != 1 || got[0].Address != "10.1.1.1:9090" {
			t.Fatalf("bare port must be completed with advertise host, got %+v", got)
		}
	})
}

// TestRegistrarStartAbortedSkipsRegister 锁定 RC-21：waitForEndpoints 因
// ctx 取消或 Stop 先行而退出时，Start 短路注册（不 tryRegister、不启动
// 后台重试），直接返回 nil。
func TestRegistrarStartAbortedSkipsRegister(t *testing.T) {
	t.Run("ctx canceled while waiting", func(t *testing.T) {
		backend := &fakeRegistry{}
		never := Static(ProtocolHTTP, "") // 永不就绪
		r := NewRegistrar(backend, WithServiceName("svc"),
			WithAdvertisers(never), WithFailFast(false))
		mustInit(t, r, newFakeAppContext())

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Start(ctx) }()
		time.Sleep(50 * time.Millisecond) // 让 Start 进入等待
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("aborted Start must return nil, got %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Start must unblock after ctx cancel")
		}
		if n := backend.registeredCount(); n != 0 {
			t.Fatalf("register must be skipped on abort, got %d calls", n)
		}
	})
	t.Run("stop while waiting", func(t *testing.T) {
		backend := &fakeRegistry{}
		never := Static(ProtocolHTTP, "")
		r := NewRegistrar(backend, WithServiceName("svc"),
			WithAdvertisers(never), WithFailFast(false))
		mustInit(t, r, newFakeAppContext())

		done := make(chan error, 1)
		go func() { done <- r.Start(context.Background()) }()
		time.Sleep(50 * time.Millisecond)
		if err := r.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("aborted Start must return nil, got %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Start must unblock after Stop")
		}
		if n := backend.registeredCount(); n != 0 {
			t.Fatalf("register must be skipped on abort, got %d calls", n)
		}
	})
}

// waitFor 轮询 cond 直到为真或超时。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
