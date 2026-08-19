package consul

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/spf13/viper"
)

// fakeConsul 是 httptest 假 Consul agent + 健康目录。
type fakeConsul struct {
	mu            sync.Mutex
	changed       chan struct{}
	index         uint64
	services      map[string][]*api.ServiceEntry
	registrations []*api.AgentServiceRegistration
	deregistered  []string
	ttlUpdates    []string
	tokens        []string
	queries       []string // RawQuery of /v1/health/service/*
}

func newFakeConsul(t *testing.T) (*fakeConsul, *httptest.Server) {
	t.Helper()
	f := &fakeConsul{
		changed:  make(chan struct{}),
		services: make(map[string][]*api.ServiceEntry),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/agent/service/register", f.handleRegister)
	mux.HandleFunc("PUT /v1/agent/service/deregister/{id}", f.handleDeregister)
	mux.HandleFunc("PUT /v1/agent/check/update/{checkid}", f.handleCheckUpdate)
	mux.HandleFunc("GET /v1/health/service/{name}", f.handleHealthService)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeConsul) bump() {
	f.index++
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeConsul) handleRegister(w http.ResponseWriter, r *http.Request) {
	var reg api.AgentServiceRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registrations = append(f.registrations, &reg)
	f.tokens = append(f.tokens, r.Header.Get("X-Consul-Token"))
	entry := &api.ServiceEntry{
		Node:    &api.Node{Node: "node1", Address: reg.Address},
		Service: regToService(&reg),
		Checks:  api.HealthChecks{{Status: api.HealthPassing}},
	}
	// upsert
	entries := f.services[reg.Name]
	for i, e := range entries {
		if e.Service.ID == reg.ID {
			entries[i] = entry
			f.services[reg.Name] = entries
			f.bump()
			return
		}
	}
	f.services[reg.Name] = append(entries, entry)
	f.bump()
}

func regToService(reg *api.AgentServiceRegistration) *api.AgentService {
	return &api.AgentService{
		ID:      reg.ID,
		Service: reg.Name,
		Tags:    reg.Tags,
		Meta:    reg.Meta,
		Port:    reg.Port,
		Address: reg.Address,
		Weights: *reg.Weights,
	}
}

func (f *fakeConsul) handleDeregister(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregistered = append(f.deregistered, id)
	for name, entries := range f.services {
		for i, e := range entries {
			if e.Service.ID == id {
				f.services[name] = append(entries[:i], entries[i+1:]...)
				f.bump()
				return
			}
		}
	}
}

func (f *fakeConsul) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttlUpdates = append(f.ttlUpdates, r.PathValue("checkid"))
}

func (f *fakeConsul) handleHealthService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	given, _ := strconv.ParseUint(r.URL.Query().Get("index"), 10, 64)
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	if wait <= 0 || wait > time.Second {
		wait = time.Second // 测试假端：blocking 上限压到 1s
	}

	f.mu.Lock()
	f.queries = append(f.queries, r.URL.RawQuery)
	if f.index <= given {
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
	}
	entries := f.services[name]
	index := f.index
	f.mu.Unlock()

	if entries == nil {
		entries = []*api.ServiceEntry{}
	}
	w.Header().Set("X-Consul-Index", strconv.FormatUint(index, 10))
	_ = json.NewEncoder(w).Encode(entries)
}

// lastRegistration 返回最后一次注册请求体。
func (f *fakeConsul) lastRegistration() *api.AgentServiceRegistration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.registrations) == 0 {
		return nil
	}
	return f.registrations[len(f.registrations)-1]
}

func newTestClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	cfg := api.DefaultConfig()
	cfg.Address = strings.TrimPrefix(srv.URL, "http://")
	cfg.Token = "test-token"
	c, err := New(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func twoEndpointInstance() registry.Instance {
	return registry.Instance{
		Name:    "svc",
		ID:      "i1",
		Version: "v1",
		Weight:  100,
		Tags:    []string{"api"},
		Meta:    map[string]string{"region": "cn-east"},
		Status:  registry.StatusPassing,
		Endpoints: []registry.Endpoint{
			{Protocol: registry.ProtocolHTTP, Address: "10.0.0.1:8080"},
			{Protocol: registry.ProtocolGRPC, Address: "10.0.0.1:9090"},
		},
	}
}

func TestRegisterBodyTTL(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv, WithCheckType(CheckTypeTTL))

	if err := c.Register(context.Background(), twoEndpointInstance()); err != nil {
		t.Fatal(err)
	}
	reg := f.lastRegistration()
	if reg == nil {
		t.Fatal("no registration received")
	}
	if reg.ID != "i1" || reg.Name != "svc" || reg.Address != "10.0.0.1" || reg.Port != 8080 {
		t.Fatalf("identity/address wrong: %+v", reg)
	}
	if len(reg.Tags) != 1 || reg.Tags[0] != "api" {
		t.Fatalf("tags wrong: %v", reg.Tags)
	}
	// Meta：version/weight/主协议透传，用户 meta 保留。
	if reg.Meta[metaVersionKey] != "v1" || reg.Meta[metaWeightKey] != "100" ||
		reg.Meta[metaMainProtocolKey] != "http" || reg.Meta["region"] != "cn-east" {
		t.Fatalf("meta wrong: %v", reg.Meta)
	}
	// 其余 Endpoint 写入 lynx_endpoints。
	var rest []registry.Endpoint
	if err := json.Unmarshal([]byte(reg.Meta[metaEndpointsKey]), &rest); err != nil {
		t.Fatalf("lynx_endpoints not JSON: %v", err)
	}
	if len(rest) != 1 || rest[0] != (registry.Endpoint{Protocol: registry.ProtocolGRPC, Address: "10.0.0.1:9090"}) {
		t.Fatalf("lynx_endpoints wrong: %v", rest)
	}
	// Weights.Passing 写入。
	if reg.Weights == nil || reg.Weights.Passing != 100 {
		t.Fatalf("weights wrong: %+v", reg.Weights)
	}
	// ttl check。
	if reg.Check == nil || reg.Check.TTL != "30s" || reg.Check.DeregisterCriticalServiceAfter != "1m0s" {
		t.Fatalf("ttl check wrong: %+v", reg.Check)
	}
	// token 进 header，且来自显式配置。
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokens) != 1 || f.tokens[0] != "test-token" {
		t.Fatalf("token header wrong: %v", f.tokens)
	}
}

func TestRegisterHTTPCheck(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv) // 默认 http check

	if err := c.Register(context.Background(), twoEndpointInstance()); err != nil {
		t.Fatal(err)
	}
	check := f.lastRegistration().Check
	if check == nil || check.HTTP != "http://10.0.0.1:8080/healthz/readiness" ||
		check.Interval != "10s" || check.Timeout != "3s" {
		t.Fatalf("http check wrong: %+v", check)
	}
	// http 被动探针：Heartbeat 是 no-op。
	if err := c.Heartbeat(context.Background(), "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ttlUpdates) != 0 {
		t.Fatalf("http check must not UpdateTTL: %v", f.ttlUpdates)
	}
}

func TestRegisterGRPCCheck(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv, WithCheckType(CheckTypeGRPC))

	inst := twoEndpointInstance()
	if err := c.Register(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	check := f.lastRegistration().Check
	if check == nil || check.GRPC != "10.0.0.1:9090" {
		t.Fatalf("grpc check wrong: %+v", check)
	}
	// grpc 主端口时 http endpoint 进 lynx_endpoints。
	var rest []registry.Endpoint
	_ = json.Unmarshal([]byte(f.lastRegistration().Meta[metaEndpointsKey]), &rest)
	if len(rest) != 1 || rest[0].Protocol != registry.ProtocolHTTP {
		t.Fatalf("rest endpoints wrong: %v", rest)
	}
}

func TestRegisterNoMatchingEndpoint(t *testing.T) {
	_, srv := newFakeConsul(t)
	c := newTestClient(t, srv, WithCheckType(CheckTypeGRPC))

	inst := twoEndpointInstance()
	inst.Endpoints = inst.Endpoints[:1] // 只剩 http
	if err := c.Register(context.Background(), inst); err == nil {
		t.Fatal("no grpc endpoint for grpc check must fail")
	}
}

func TestHeartbeatTTLUpdate(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv, WithCheckType(CheckTypeTTL))

	if err := c.Heartbeat(context.Background(), "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ttlUpdates) != 1 || f.ttlUpdates[0] != "service:i1" {
		t.Fatalf("ttl update wrong: %v", f.ttlUpdates)
	}
}

func TestGetServiceDecodesEndpoints(t *testing.T) {
	_, srv := newFakeConsul(t)
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
	inst := got[0]
	// 主端口在首位，lynx_endpoints 还原在其次。
	if len(inst.Endpoints) != 2 ||
		inst.Endpoints[0] != (registry.Endpoint{Protocol: registry.ProtocolHTTP, Address: "10.0.0.1:8080"}) ||
		inst.Endpoints[1] != (registry.Endpoint{Protocol: registry.ProtocolGRPC, Address: "10.0.0.1:9090"}) {
		t.Fatalf("endpoints not restored: %+v", inst.Endpoints)
	}
	if inst.Version != "v1" || inst.Weight != 100 || inst.Status != registry.StatusPassing {
		t.Fatalf("fields not restored: %+v", inst)
	}
	// 内部键不进入 Meta，用户 meta 保留。
	if _, ok := inst.Meta[metaEndpointsKey]; ok {
		t.Fatalf("internal key leaked: %v", inst.Meta)
	}
	if inst.Meta["region"] != "cn-east" || len(inst.Meta) != 1 {
		t.Fatalf("user meta wrong: %v", inst.Meta)
	}
}

func TestGetServiceFiltersCritical(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv)

	// 直接往假目录塞一条 critical 实例。
	f.mu.Lock()
	f.services["svc"] = []*api.ServiceEntry{{
		Node: &api.Node{Node: "n1", Address: "10.0.0.9"},
		Service: &api.AgentService{ID: "bad", Service: "svc", Address: "10.0.0.9", Port: 8080,
			Meta: map[string]string{metaMainProtocolKey: "http"}},
		Checks: api.HealthChecks{{Status: api.HealthCritical}},
	}}
	f.bump()
	f.mu.Unlock()

	got, err := c.GetService(context.Background(), "svc", registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("critical must be filtered by default: %+v", got)
	}
	got, err = c.GetService(context.Background(), "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != registry.StatusCritical {
		t.Fatalf("IncludeUnhealthy must return critical: %+v", got)
	}
}

func TestWatchBlockingQuery(t *testing.T) {
	_, srv := newFakeConsul(t)
	c := newTestClient(t, srv)
	ctx := context.Background()

	w, err := c.Watch(ctx, "svc", registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	// 首次 Next 立即给当前快照（空）。
	snap, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("want empty first snapshot, got %d", len(snap))
	}

	// 注册一个实例 → blocking query 推进，Next 返回新快照。
	if err := c.Register(ctx, twoEndpointInstance()); err != nil {
		t.Fatal(err)
	}
	done := make(chan []registry.Instance, 1)
	go func() {
		s, err := w.Next()
		if err != nil {
			t.Error(err)
			return
		}
		done <- s
	}()
	select {
	case s := <-done:
		if len(s) != 1 || s[0].ID != "i1" {
			t.Fatalf("watch push wrong: %+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not push after register")
	}
}

func TestDeregisterIdempotent(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv)

	ctx := context.Background()
	if err := c.Register(ctx, twoEndpointInstance()); err != nil {
		t.Fatal(err)
	}
	if err := c.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deregistered) != 2 || f.deregistered[0] != "i1" {
		t.Fatalf("deregister calls wrong: %v", f.deregistered)
	}
	if len(f.services["svc"]) != 0 {
		t.Fatal("service must be removed from catalog")
	}
}

func TestAllowStaleDefaultFalse(t *testing.T) {
	f, srv := newFakeConsul(t)
	c := newTestClient(t, srv)

	if _, err := c.GetService(context.Background(), "svc", registry.Filter{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if strings.Contains(f.queries[len(f.queries)-1], "stale") {
		t.Fatalf("default must be consistent read: %s", f.queries[len(f.queries)-1])
	}
	f.mu.Unlock()

	c2 := newTestClient(t, srv, WithAllowStale(true))
	if _, err := c2.GetService(context.Background(), "svc", registry.Filter{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.queries[len(f.queries)-1], "stale") {
		t.Fatalf("allow_stale=true must add stale param: %s", f.queries[len(f.queries)-1])
	}
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv(tokenEnv, "env-token")
	f, srv := newFakeConsul(t)

	cfg := api.DefaultConfig()
	cfg.Address = strings.TrimPrefix(srv.URL, "http://")
	// 配置 token 为空 → 直读 CONSUL_HTTP_TOKEN。
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Register(context.Background(), twoEndpointInstance()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokens) != 1 || f.tokens[0] != "env-token" {
		t.Fatalf("env token not used: %v", f.tokens)
	}
}

func TestCloseIdempotent(t *testing.T) {
	_, srv := newFakeConsul(t)
	cfg := api.DefaultConfig()
	cfg.Address = strings.TrimPrefix(srv.URL, "http://")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.Register(context.Background(), twoEndpointInstance()); !errors.Is(err, errClosed) {
		t.Fatalf("Register after Close: %v", err)
	}
}

func consulConfig(t *testing.T, section map[string]any) lynx.Config {
	t.Helper()
	v := viper.New()
	if section != nil {
		v.Set("registry", section)
	}
	return lynx.NewViperConfig(v)
}

func TestNewFromConfig(t *testing.T) {
	t.Run("section missing", func(t *testing.T) {
		c, err := NewFromConfig(consulConfig(t, nil))
		if err != nil || c != nil {
			t.Fatalf("want (nil,nil), got %v %v", c, err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		c, err := NewFromConfig(consulConfig(t, map[string]any{
			"enabled": false, "backend": "consul",
		}))
		if err != nil || c != nil {
			t.Fatalf("want (nil,nil), got %v %v", c, err)
		}
	})
	t.Run("backend not consul", func(t *testing.T) {
		c, err := NewFromConfig(consulConfig(t, map[string]any{"backend": "memory"}))
		if err != nil || c != nil {
			t.Fatalf("want (nil,nil), got %v %v", c, err)
		}
	})
	t.Run("consul with url address and tls", func(t *testing.T) {
		c, err := NewFromConfig(consulConfig(t, map[string]any{
			"backend":          "consul",
			"heartbeat_ttl":    "45s",
			"deregister_after": "90s",
			"consul": map[string]any{
				"address":     "http://127.0.0.1:8500",
				"datacenter":  "dc1",
				"namespace":   "team-a",
				"allow_stale": true,
				"tls":         map[string]any{"enabled": true, "insecure_skip_verify": true},
			},
			"health_check": map[string]any{"type": "ttl"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			t.Fatal("want Client")
		}
		defer func() { _ = c.Close() }()
		if c.heartbeatTTL != 45*time.Second || c.deregisterAfter != 90*time.Second ||
			c.checkType != CheckTypeTTL || !c.allowStale {
			t.Fatalf("config not mapped: %+v", c)
		}
	})
	t.Run("invalid address", func(t *testing.T) {
		_, err := NewFromConfig(consulConfig(t, map[string]any{
			"backend": "consul",
			"consul":  map[string]any{"address": "http://"},
		}))
		if err == nil {
			t.Fatal("invalid address must fail")
		}
	})
}
