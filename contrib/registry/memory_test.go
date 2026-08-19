package registry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func passing(name, id string, endpoints ...Endpoint) Instance {
	return Instance{
		Name:      name,
		ID:        id,
		Version:   "v1",
		Status:    StatusPassing,
		Endpoints: endpoints,
	}
}

func TestMemoryRegisterUpsert(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	inst := passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})
	if err := m.Register(ctx, inst); err != nil {
		t.Fatal(err)
	}

	// 同 ID 二次注册：last-write-wins upsert。
	inst.Endpoints = []Endpoint{{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"}}
	if err := m.Register(ctx, inst); err != nil {
		t.Fatal(err)
	}

	got, err := m.GetService(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 instance, got %d", len(got))
	}
	if got[0].Endpoints[0].Address != "10.0.0.2:8080" {
		t.Fatalf("upsert not applied: %+v", got[0].Endpoints)
	}
}

func TestMemoryFilterProtocol(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.Register(ctx, passing("svc", "http-only", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})))
	must(m.Register(ctx, passing("svc", "grpc-only", Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.2:9090"})))
	must(m.Register(ctx, passing("svc", "both",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.3:8080"},
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.3:9090"},
	)))

	got, err := m.GetService(ctx, "svc", Filter{Protocol: ProtocolGRPC})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("protocol filter: want 2, got %d (%v)", len(got), ids(got))
	}
}

func TestMemoryFilterTags(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	a := passing("svc", "a", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})
	a.Tags = []string{"blue", "v2"}
	b := passing("svc", "b", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"})
	b.Tags = []string{"blue"}
	if err := m.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(ctx, b); err != nil {
		t.Fatal(err)
	}

	// Tags 必须全匹配。
	got, err := m.GetService(ctx, "svc", Filter{Tags: []string{"blue", "v2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("tags all-match: want [a], got %v", ids(got))
	}

	got, err = m.GetService(ctx, "svc", Filter{Tags: []string{"blue"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("tags subset: want 2, got %v", ids(got))
	}
}

func TestMemoryFilterStatus(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	ok := passing("svc", "ok", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})
	bad := passing("svc", "bad", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"})
	bad.Status = StatusCritical
	zero := passing("svc", "zero", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.3:8080"})
	zero.Status = StatusUnknown
	for _, inst := range []Instance{ok, bad, zero} {
		if err := m.Register(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}

	// 零值 Filter 只返回 StatusPassing。
	got, err := m.GetService(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("default filter: want [ok], got %v", ids(got))
	}

	// IncludeUnhealthy=true 全返回。
	got, err = m.GetService(ctx, "svc", Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("include unhealthy: want 3, got %v", ids(got))
	}
}

func TestMemorySnapshotDeepCopy(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	inst := passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})
	inst.Tags = []string{"blue"}
	inst.Meta = map[string]string{"k": "v"}
	if err := m.Register(ctx, inst); err != nil {
		t.Fatal(err)
	}
	// 注册后调用方改原实例不影响目录。
	inst.Endpoints[0].Address = "changed:1"
	inst.Meta["k"] = "changed"

	got, err := m.GetService(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// 篡改返回的快照。
	got[0].Endpoints[0].Address = "hacked:1"
	got[0].Tags[0] = "hacked"
	got[0].Meta["k"] = "hacked"

	again, err := m.GetService(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Endpoints[0].Address != "10.0.0.1:8080" ||
		again[0].Tags[0] != "blue" || again[0].Meta["k"] != "v" {
		t.Fatalf("snapshot mutated: %+v", again[0])
	}
}

// TestMemoryMultiEndpointNative 证明 memory 原生存取一条 Instance 挂多个
// Endpoint，不经任何 Meta/JSON 编码：Resolver 后续单测据此与 Consul Meta
// 编码解耦。
func TestMemoryMultiEndpointNative(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	inst := passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"},
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"},
	)
	if err := m.Register(ctx, inst); err != nil {
		t.Fatal(err)
	}

	got, err := m.GetService(ctx, "svc", Filter{Protocol: ProtocolGRPC})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Endpoints) != 2 {
		t.Fatalf("want 1 instance with 2 endpoints, got %+v", got)
	}
	if got[0].Endpoints[1] != (Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"}) {
		t.Fatalf("endpoint order/content changed: %+v", got[0].Endpoints)
	}
	if len(got[0].Meta) != 0 {
		t.Fatalf("Meta 不应被用于编码 endpoints: %+v", got[0].Meta)
	}
}

func TestMemoryWatchInitialSnapshotEmpty(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	w, err := m.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	// 首次 Next 立即返回当前快照，空服务也要立即给空列表。
	snap, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("want empty initial snapshot, got %v", ids(snap))
	}
}

func TestMemoryWatchPushesChanges(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	w, err := m.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	if _, err := w.Next(); err != nil { // 首快照（空）
		t.Fatal(err)
	}

	if err := m.Register(ctx, passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})); err != nil {
		t.Fatal(err)
	}
	snap, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 1 || snap[0].ID != "i1" {
		t.Fatalf("want [i1], got %v", ids(snap))
	}

	// 空快照立即生效：Deregister 后推送空列表，不回退旧非空列表。
	if err := m.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	snap, err = w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("want empty snapshot after deregister, got %v", ids(snap))
	}
}

func TestMemoryWatchStop(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()

	w, err := m.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Next(); err != nil { // 首快照
		t.Fatal(err)
	}
	if err := w.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Next(); err == nil {
		t.Fatal("Next after Stop must return error")
	}
	// Stop 幂等。
	if err := w.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestMemoryWatchContextCancel(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	w, err := m.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()
	if _, err := w.Next(); err != nil { // 首快照
		t.Fatal(err)
	}
	cancel()
	if _, err := w.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestMemoryDeregisterAndCloseIdempotent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	// Deregister 不存在的实例：no-op。
	if err := m.Deregister(ctx, "svc", "nope"); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(ctx, passing("svc", "i1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Close 后写入被拒绝。
	if err := m.Register(ctx, passing("svc", "i2")); err == nil {
		t.Fatal("Register after Close must fail")
	}
}

func TestMemoryWatchUnblocksOnClose(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	w, err := m.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Next(); err != nil { // 首快照
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Next()
		done <- err
	}()
	// 给 Next 进入阻塞的时间。
	time.Sleep(20 * time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Next after Close must return error")
		}
	case <-time.After(time.Second):
		t.Fatal("Next did not unblock on Close")
	}
}

func ids(instances []Instance) []string {
	out := make([]string, 0, len(instances))
	for _, inst := range instances {
		out = append(out, inst.ID)
	}
	return out
}
