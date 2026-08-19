package registry

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// fakeClientConn 记录 UpdateState 收到的 resolver.State。
type fakeClientConn struct {
	mu     sync.Mutex
	states []resolver.State
	errs   []error
}

func (c *fakeClientConn) UpdateState(s resolver.State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = append(c.states, s)
	return nil
}

func (c *fakeClientConn) ReportError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, err)
}

func (c *fakeClientConn) NewAddress([]resolver.Address) {}

func (c *fakeClientConn) ParseServiceConfig(string) *serviceconfig.ParseResult {
	return &serviceconfig.ParseResult{}
}

func (c *fakeClientConn) lastState() resolver.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.states) == 0 {
		return resolver.State{}
	}
	return c.states[len(c.states)-1]
}

func (c *fakeClientConn) stateCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.states)
}

// newGRPCBuilderForTest 返回 pollInterval 调小的 Builder 与 memory 后端。
func newGRPCBuilderForTest(t *testing.T, m *Memory) resolver.Builder {
	t.Helper()
	rslv := NewResolver(m)
	t.Cleanup(func() { _ = rslv.Close() })
	b := NewGRPCBuilder(rslv).(*grpcBuilder)
	b.pollInterval = 10 * time.Millisecond
	return b
}

func mustParseTarget(t *testing.T, raw string) resolver.Target {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return resolver.Target{URL: *u}
}

func TestGRPCBuilderScheme(t *testing.T) {
	rslv := NewResolver(NewMemory())
	defer func() { _ = rslv.Close() }()
	if got := NewGRPCBuilder(rslv).Scheme(); got != "registry" {
		t.Fatalf("want scheme registry, got %q", got)
	}
}

func TestGRPCBuilderRejectsAuthorityForm(t *testing.T) {
	rslv := NewResolver(NewMemory())
	defer func() { _ = rslv.Close() }()
	b := NewGRPCBuilder(rslv)
	// registry://svc/grpc 的 authority 形式不支持：Host 非空 → error。
	if _, err := b.Build(mustParseTarget(t, "registry://svc/grpc"), &fakeClientConn{}, resolver.BuildOptions{}); err == nil {
		t.Fatal("want error for authority-form target")
	}
}

func TestGRPCBuilderRejectsBadTarget(t *testing.T) {
	rslv := NewResolver(NewMemory())
	defer func() { _ = rslv.Close() }()
	b := NewGRPCBuilder(rslv)
	cc := &fakeClientConn{}

	// 空服务名。
	if _, err := b.Build(mustParseTarget(t, "registry:///"), cc, resolver.BuildOptions{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("empty name: want ErrBadName, got %v", err)
	}
	// 服务名含多余路径段。
	if _, err := b.Build(mustParseTarget(t, "registry:///a/b"), cc, resolver.BuildOptions{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("nested path: want ErrBadName, got %v", err)
	}
	// protocol 只接受 grpc。
	if _, err := b.Build(mustParseTarget(t, "registry:///svc?protocol=http"), cc, resolver.BuildOptions{}); !errors.Is(err, ErrBadProtocol) {
		t.Fatalf("protocol=http: want ErrBadProtocol, got %v", err)
	}
}

func TestGRPCResolverPublishesAddresses(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	inst := passing("svc", "i1",
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"},
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}, // 非 grpc 不入选
	)
	inst.Weight = 150
	inst.Version = "v2"
	if err := m.Register(context.Background(), inst); err != nil {
		t.Fatal(err)
	}

	b := newGRPCBuilderForTest(t, m)
	cc := &fakeClientConn{}
	r, err := b.Build(mustParseTarget(t, "registry:///svc"), cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	eventually(t, "initial addresses published", func() bool {
		return len(cc.lastState().Addresses) == 1
	})
	addr := cc.lastState().Addresses[0]
	if addr.Addr != "10.0.0.1:9090" {
		t.Fatalf("want grpc endpoint addr, got %q", addr.Addr)
	}
	if got := addr.Attributes.Value("weight"); got != 150 {
		t.Fatalf("weight attribute: want 150, got %v", got)
	}
	if got := addr.Attributes.Value("version"); got != "v2" {
		t.Fatalf("version attribute: want v2, got %v", got)
	}
}

func TestGRPCResolverFollowsUpdates(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	if err := m.Register(ctx, passing("svc", "i1",
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"})); err != nil {
		t.Fatal(err)
	}

	b := newGRPCBuilderForTest(t, m)
	cc := &fakeClientConn{}
	r, err := b.Build(mustParseTarget(t, "registry:///svc"), cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	eventually(t, "initial address", func() bool {
		return len(cc.lastState().Addresses) == 1
	})

	// 新实例注册后，轮询拾取缓存变化并 UpdateState。
	if err := m.Register(ctx, passing("svc", "i2",
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.2:9090"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second address published", func() bool {
		return len(cc.lastState().Addresses) == 2
	})

	// ResolveNow 触发立即再解析。
	if err := m.Deregister(ctx, "svc", "i2"); err != nil {
		t.Fatal(err)
	}
	r.ResolveNow(resolver.ResolveNowOptions{})
	eventually(t, "deregistration published", func() bool {
		return len(cc.lastState().Addresses) == 1
	})
}

func TestGRPCResolverEmptySnapshotPublished(t *testing.T) {
	// 服务下线：空列表同样 UpdateState（空快照立即生效）。
	m := NewMemory()
	defer func() { _ = m.Close() }()
	b := newGRPCBuilderForTest(t, m)
	cc := &fakeClientConn{}
	r, err := b.Build(mustParseTarget(t, "registry:///svc"), cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	eventually(t, "empty state published", func() bool {
		return cc.stateCount() > 0
	})
	if got := cc.lastState().Addresses; len(got) != 0 {
		t.Fatalf("want 0 addresses, got %d", len(got))
	}
}

func TestGRPCResolverKeepsLastStateOnStaleDrop(t *testing.T) {
	// 快照超 stale 上限被丢弃：GetAll 报 ErrNoInstance，按 gRPC 惯例
	// 保留上一次地址、不清空。
	f := &fakeDiscovery{snap: []Instance{passing("svc", "i1",
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"})}}
	rslv := NewResolver(f, WithStaleMaxAge(100*time.Millisecond))
	defer func() { _ = rslv.Close() }()
	b := NewGRPCBuilder(rslv).(*grpcBuilder)
	b.pollInterval = 10 * time.Millisecond

	cc := &fakeClientConn{}
	r, err := b.Build(mustParseTarget(t, "registry:///svc"), cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	eventually(t, "watcher established", func() bool {
		return f.watcherCount() > 0
	})
	eventually(t, "initial address", func() bool {
		return len(cc.lastState().Addresses) == 1
	})

	f.breakWatch()
	// 等 stale 过期后多轮 poll：地址必须保持最后一次成功状态。
	time.Sleep(300 * time.Millisecond)
	if got := cc.lastState().Addresses; len(got) != 1 || got[0].Addr != "10.0.0.1:9090" {
		t.Fatalf("must keep last state after stale drop, got %+v", got)
	}
}

func TestGRPCResolverCloseIdempotent(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	b := newGRPCBuilderForTest(t, m)
	r, err := b.Build(mustParseTarget(t, "registry:///svc"), &fakeClientConn{}, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	r.Close() // 幂等
}
