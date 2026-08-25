package registry

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeLookup 是可编程的 lookupResolver，无真实网络。
type fakeLookup struct {
	mu        sync.Mutex
	srv       map[string][]*net.SRV // key: service+"|"+name
	srvErr    error                 // 非 nil 时所有 SRV 查询返回它
	hosts     []string
	hostErr   error
	srvCalls  int
	hostCalls int
}

func nxdomainErr() error {
	return &net.DNSError{IsNotFound: true, Name: "fake", IsTimeout: false}
}

func (f *fakeLookup) LookupSRV(_ context.Context, service, _, name string) (string, []*net.SRV, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.srvCalls++
	if f.srvErr != nil {
		return "", nil, f.srvErr
	}
	records, ok := f.srv[service+"|"+name]
	if !ok {
		return "", nil, nxdomainErr()
	}
	return "", records, nil
}

func (f *fakeLookup) LookupHost(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostCalls++
	if f.hostErr != nil {
		return nil, f.hostErr
	}
	return append([]string(nil), f.hosts...), nil
}

func (f *fakeLookup) hostCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostCalls
}

func (f *fakeLookup) setHosts(hosts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = hosts
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{srv: make(map[string][]*net.SRV)}
}

func TestDNSSRVPreferred(t *testing.T) {
	lookup := newFakeLookup()
	lookup.srv["_http|svc.default.svc.cluster.local"] = []*net.SRV{
		{Target: "pod-b.default.svc.cluster.local.", Port: 8081},
		{Target: "pod-a.default.svc.cluster.local.", Port: 8080},
	}
	lookup.hosts = []string{"10.0.0.1"} // SRV 命中时不应查 A

	d := NewDNSDiscovery(withDNSLookup(lookup))
	got, err := d.GetService(context.Background(), "svc", Filter{Protocol: ProtocolHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 instances, got %v", ids(got))
	}
	// 按 target 排序：pod-a 在前；target 尾点被去掉。
	if got[0].ID != "pod-a.default.svc.cluster.local" ||
		got[0].Endpoints[0] != (Endpoint{Protocol: ProtocolHTTP, Address: "pod-a.default.svc.cluster.local:8080"}) {
		t.Fatalf("unexpected SRV instance: %+v", got[0])
	}
	if lookup.hostCallCount() != 0 {
		t.Fatal("SRV hit must not fall back to A/AAAA")
	}
	if got[0].Status != StatusPassing {
		t.Fatalf("DNS instances are always Passing, got %v", got[0].Status)
	}
}

// TestDNSNameConsistentAcrossPaths 锁定 RC-14：SRV 路径与 Host 路径的
// 快照 Name 统一为 FQDN（查询名），同一服务在两种路径间切换时 Name
// 不跳变、快照可比较。
func TestDNSNameConsistentAcrossPaths(t *testing.T) {
	lookup := newFakeLookup()
	// svc 走 SRV 路径；plain 无 SRV 记录，回落 A/AAAA。
	lookup.srv["_http|svc.default.svc.cluster.local"] = []*net.SRV{
		{Target: "pod-a.default.svc.cluster.local.", Port: 8080},
	}
	lookup.hosts = []string{"10.0.0.1"}

	d := NewDNSDiscovery(withDNSLookup(lookup))
	srvGot, err := d.GetService(context.Background(), "svc", Filter{Protocol: ProtocolHTTP})
	if err != nil {
		t.Fatal(err)
	}
	hostGot, err := d.GetService(context.Background(), "plain", Filter{Protocol: ProtocolHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if len(srvGot) != 1 || len(hostGot) != 1 {
		t.Fatalf("want 1 instance per path, got srv=%v host=%v", ids(srvGot), ids(hostGot))
	}
	if want := "svc.default.svc.cluster.local"; srvGot[0].Name != want {
		t.Fatalf("SRV path Name = %q, want FQDN %q", srvGot[0].Name, want)
	}
	if want := "plain.default.svc.cluster.local"; hostGot[0].Name != want {
		t.Fatalf("Host path Name = %q, want FQDN %q (与 SRV 路径形态一致)", hostGot[0].Name, want)
	}
}

func TestDNSFallbackToHostWithPorts(t *testing.T) {
	lookup := newFakeLookup() // 无 SRV → NXDOMAIN
	lookup.hosts = []string{"10.0.0.2", "10.0.0.1"}

	d := NewDNSDiscovery(withDNSLookup(lookup))
	got, err := d.GetService(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 instances (one per IP), got %v", ids(got))
	}
	// 一条 A 记录 + 多协议 = 多条 Endpoint（同 host 不同 port）。
	// 协议按字典序：grpc, http, https。
	inst := got[0]
	if inst.ID != "10.0.0.1" {
		t.Fatalf("instances must be sorted by IP, got %v", ids(got))
	}
	want := []Endpoint{
		{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"},
		{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"},
		{Protocol: ProtocolHTTPS, Address: "10.0.0.1:8443"},
	}
	if !equalDNSSnapshots([]Instance{{ID: inst.ID, Endpoints: inst.Endpoints}},
		[]Instance{{ID: inst.ID, Endpoints: want}}) {
		t.Fatalf("unexpected endpoints: %+v", inst.Endpoints)
	}
}

func TestDNSProtocolFilterAndPortsOverride(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hosts = []string{"10.0.0.1"}

	d := NewDNSDiscovery(withDNSLookup(lookup), WithDNSPorts(map[string]int{ProtocolGRPC: 19090}))
	got, err := d.GetService(context.Background(), "svc", Filter{Protocol: ProtocolGRPC})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Endpoints) != 1 {
		t.Fatalf("protocol filter must yield single-endpoint instance: %+v", got)
	}
	if got[0].Endpoints[0] != (Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:19090"}) {
		t.Fatalf("ports override not applied: %+v", got[0].Endpoints[0])
	}
}

func TestDNSNotFoundReturnsEmpty(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hostErr = nxdomainErr()

	d := NewDNSDiscovery(withDNSLookup(lookup))
	got, err := d.GetService(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatalf("NXDOMAIN must be (empty, nil), got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", ids(got))
	}
}

func TestDNSEmptyName(t *testing.T) {
	d := NewDNSDiscovery(withDNSLookup(newFakeLookup()))
	if _, err := d.GetService(context.Background(), "", Filter{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("want ErrBadName, got %v", err)
	}
	if _, err := d.Watch(context.Background(), "", Filter{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("want ErrBadName, got %v", err)
	}
}

func TestDNSTransientErrorPropagates(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hostErr = &net.DNSError{IsTimeout: true, Name: "fake"}

	d := NewDNSDiscovery(withDNSLookup(lookup))
	if _, err := d.GetService(context.Background(), "svc", Filter{}); err == nil {
		t.Fatal("transient DNS error must propagate")
	}
}

func TestNegativeCacheDelayClamp(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		time.Second:      negativeCacheMin, // 1s → 5s
		15 * time.Second: 15 * time.Second, // 区间内不变
		time.Minute:      negativeCacheMax, // 60s → 30s
	}
	for in, want := range cases {
		if got := negativeCacheDelay(in); got != want {
			t.Fatalf("negativeCacheDelay(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestDNSWatchFirstSnapshotAndChanges(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hosts = []string{"10.0.0.1"}

	d := NewDNSDiscovery(withDNSLookup(lookup), WithDNSPollInterval(30*time.Millisecond))
	w, err := d.Watch(context.Background(), "svc", Filter{Protocol: ProtocolHTTP})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	// 首次 Next 立即给当前快照。
	snap, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 1 || snap[0].ID != "10.0.0.1" {
		t.Fatalf("first snapshot: %+v", snap)
	}

	// 集合变化才推送。
	lookup.setHosts([]string{"10.0.0.1", "10.0.0.2"})
	snap, err = w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("change push: %+v", ids(snap))
	}

	// 空快照（NXDOMAIN）也要推送。
	lookup.mu.Lock()
	lookup.hostErr = nxdomainErr()
	lookup.mu.Unlock()
	snap, err = w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("empty snapshot must be pushed, got %+v", ids(snap))
	}
}

func TestDNSWatchFirstSnapshotEmpty(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hostErr = nxdomainErr()

	d := NewDNSDiscovery(withDNSLookup(lookup), WithDNSPollInterval(30*time.Millisecond))
	w, err := d.Watch(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	snap, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("want empty first snapshot, got %+v", ids(snap))
	}
}

func TestDNSWatchNegativeCacheBackoff(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hostErr = nxdomainErr()

	// poll_interval 远小于负缓存下限：失败后不得按 30ms 继续狂查。
	d := NewDNSDiscovery(withDNSLookup(lookup), WithDNSPollInterval(30*time.Millisecond))
	w, err := d.Watch(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	if _, err := w.Next(); err != nil { // 首快照（空，含一次 host 查询）
		t.Fatal(err)
	}
	// 首轮轮询仍按 poll_interval 触发一次（30ms），此后进入负缓存钳制。
	time.Sleep(150 * time.Millisecond)
	calls := lookup.hostCallCount()
	time.Sleep(200 * time.Millisecond)
	if got := lookup.hostCallCount(); got != calls {
		t.Fatalf("negative cache must clamp retry to [5s,30s]: hostCalls %d -> %d", calls, got)
	}
}

func TestDNSWatchStopIdempotentAndCtxCancel(t *testing.T) {
	lookup := newFakeLookup()
	lookup.hosts = []string{"10.0.0.1"}

	ctx, cancel := context.WithCancel(context.Background())
	d := NewDNSDiscovery(withDNSLookup(lookup), WithDNSPollInterval(time.Hour))

	// Stop 幂等 + Stop 后 Next 返回错误。
	w, err := d.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Next(); err != nil { // 首快照
		t.Fatal(err)
	}
	if err := w.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, err := w.Next(); err == nil {
		t.Fatal("Next after Stop must return error")
	}

	// ctx 取消后 Next 返回错误。
	w2, err := d.Watch(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Stop() }()
	if _, err := w2.Next(); err != nil { // 首快照
		t.Fatal(err)
	}
	cancel()
	if _, err := w2.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
