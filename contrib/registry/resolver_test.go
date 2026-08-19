package registry

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeDiscovery 是可控的 Discovery：可注入 Watch 失败、让现有 Watcher
// 报错、推送新快照，用于测试 stale / 重连 / 轮询回退路径。
type fakeDiscovery struct {
	mu       sync.Mutex
	snap     []Instance
	watchErr error // 非 nil 时 Watch 直接失败
	watchers []*fakeWatcher
}

var errWatchBroken = errors.New("registry: fake watch broken")

func (f *fakeDiscovery) GetService(_ context.Context, _ string, _ Filter) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.snap), nil
}

func (f *fakeDiscovery) Watch(ctx context.Context, _ string, _ Filter) (Watcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	w := &fakeWatcher{
		ctx:  ctx,
		ch:   make(chan []Instance, 1),
		done: make(chan struct{}),
	}
	// 对齐 Watcher 契约：首次 Next 立即推送当前快照。
	w.ch <- slices.Clone(f.snap)
	f.watchers = append(f.watchers, w)
	return w, nil
}

// push 更新后端快照并推送给所有活跃 Watcher。
func (f *fakeDiscovery) push(snap []Instance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	for _, w := range f.watchers {
		select {
		case w.ch <- slices.Clone(snap):
		default:
		}
	}
}

// breakWatch 让所有现有 Watcher 的 Next 报错，并使后续 Watch 失败。
func (f *fakeDiscovery) breakWatch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchErr = errWatchBroken
	for _, w := range f.watchers {
		w.fail(errWatchBroken)
	}
}

// healWatch 恢复 Watch（模拟重连成功）。
func (f *fakeDiscovery) healWatch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchErr = nil
}

// watcherCount 返回已建立的 Watcher 数，用于测试同步（等待 watch 就绪）。
func (f *fakeDiscovery) watcherCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers)
}

type fakeWatcher struct {
	ctx  context.Context
	ch   chan []Instance
	done chan struct{}
	once sync.Once

	mu  sync.Mutex
	err error
}

func (w *fakeWatcher) fail(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

func (w *fakeWatcher) Next() ([]Instance, error) {
	w.mu.Lock()
	err := w.err
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case snap := <-w.ch:
		return snap, nil
	case <-w.done:
		return nil, errors.New("registry: fake watcher stopped")
	case <-w.ctx.Done():
		return nil, w.ctx.Err()
	}
}

func (w *fakeWatcher) Stop() error {
	w.once.Do(func() { close(w.done) })
	return nil
}

// eventually 在 deadline 内反复调用 cond 直到为真。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestResolverGetInitialSyncFetch(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	if err := m.Register(ctx, passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})); err != nil {
		t.Fatal(err)
	}

	r := NewResolver(m)
	defer func() { _ = r.Close() }()

	// 缓存未填充：首个 Get 同步 GetService，不等待 watch 首轮。
	inst, err := r.Get(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != "i1" {
		t.Fatalf("want i1, got %q", inst.ID)
	}
}

func TestResolverGetBadName(t *testing.T) {
	r := NewResolver(NewMemory())
	defer func() { _ = r.Close() }()
	ctx := context.Background()

	if _, err := r.Get(ctx, "", Filter{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("Get: want ErrBadName, got %v", err)
	}
	if _, err := r.GetAll(ctx, "", Filter{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("GetAll: want ErrBadName, got %v", err)
	}
}

func TestResolverGetAllEmptyReturnsNilError(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	r := NewResolver(m)
	defer func() { _ = r.Close() }()

	// 后端空切片 = 「当前无实例」：([], nil)，无 ErrNotFound。
	got, err := r.GetAll(context.Background(), "ghost", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 instances, got %d", len(got))
	}
	// Get 在过滤后为空时返回 ErrNoInstance。
	if _, err := r.Get(context.Background(), "ghost", Filter{}); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("want ErrNoInstance, got %v", err)
	}
}

func TestResolverWatchPushesUpdate(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	if err := m.Register(ctx, passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(m)
	defer func() { _ = r.Close() }()

	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatal(err)
	}
	// 注册新实例：Watch 推送应更新缓存，无需再次同步拉取。
	if err := m.Register(ctx, passing("svc", "i2",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "watch push updates cache", func() bool {
		got, err := r.GetAll(ctx, "svc", Filter{})
		return err == nil && len(got) == 2
	})
}

func TestResolverEmptySnapshotTakesEffectImmediately(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	if err := m.Register(ctx, passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(m)
	defer func() { _ = r.Close() }()

	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatal(err)
	}
	// 服务下线：空快照立即生效，不得回退旧非空列表。
	if err := m.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "empty snapshot takes effect", func() bool {
		_, err := r.Get(ctx, "svc", Filter{})
		return errors.Is(err, ErrNoInstance)
	})
	got, err := r.GetAll(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 instances after deregister, got %d", len(got))
	}
}

func TestResolverStaleWhileRevalidate(t *testing.T) {
	f := &fakeDiscovery{snap: []Instance{passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})}}
	r := NewResolver(f, WithStaleMaxAge(100*time.Millisecond), WithPollInterval(50*time.Millisecond))
	defer func() { _ = r.Close() }()
	ctx := context.Background()

	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatal(err)
	}
	// 等 watch goroutine 建立 Watcher，再断开：否则首次 Watch 落在
	// breakWatch 之后会走轮询路径，刷新 updatedAt，stale 永不触发。
	eventually(t, "watcher established", func() bool {
		return f.watcherCount() > 0
	})

	// Watch 断开且重建失败：stale 期内仍提供最后一次成功快照。
	f.breakWatch()
	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatalf("stale window: want last snapshot, got %v", err)
	}

	// 超过 stale_max_age：快照被丢弃，Get 返回 ErrNoInstance。
	eventually(t, "stale snapshot dropped", func() bool {
		_, err := r.Get(ctx, "svc", Filter{})
		return errors.Is(err, ErrNoInstance)
	})

	// Watch 恢复后重连推送新快照（退避首档 1s，给足时间），缓存恢复可读。
	f.healWatch()
	f.push([]Instance{passing("svc", "i2",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"})})
	eventually(t, "watch reconnect repopulates cache", func() bool {
		inst, err := r.Get(ctx, "svc", Filter{})
		return err == nil && inst.ID == "i2"
	})
}

func TestResolverPollFallbackWhenWatchUnavailable(t *testing.T) {
	f := &fakeDiscovery{
		snap: []Instance{passing("svc", "i1",
			Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})},
		watchErr: errWatchBroken, // Watch 从一开始就不可用
	}
	r := NewResolver(f, WithPollInterval(10*time.Millisecond))
	defer func() { _ = r.Close() }()
	ctx := context.Background()

	// 首次同步拉取已填充缓存。
	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatal(err)
	}
	// Watch 不可用期间按 pollInterval GetService：后端变化仍被拾取。
	f.push([]Instance{
		passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}),
		passing("svc", "i2", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"}),
	})
	eventually(t, "poll fallback refreshes cache", func() bool {
		got, err := r.GetAll(ctx, "svc", Filter{})
		return err == nil && len(got) == 2
	})
}

func TestResolverFilterAppliedOnReadPath(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.Register(ctx, passing("svc", "http",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})))
	must(m.Register(ctx, passing("svc", "grpc",
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.2:9090"})))

	// 缓存键只有服务名：同一 Resolver 上不同 Filter 共享一条缓存。
	r := NewResolver(m)
	defer func() { _ = r.Close() }()

	got, err := r.GetAll(ctx, "svc", Filter{Protocol: ProtocolGRPC})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "grpc" {
		t.Fatalf("protocol filter: got %+v", got)
	}
	all, err := r.GetAll(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("shared cache: want 2 instances, got %d", len(all))
	}
}

func TestResolverDefaultPickerRoundRobin(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	for _, id := range []string{"i1", "i2"} {
		if err := m.Register(ctx, passing("svc", id,
			Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})); err != nil {
			t.Fatal(err)
		}
	}
	r := NewResolver(m)
	defer func() { _ = r.Close() }()

	first, err := r.Get(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Get(ctx, "svc", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("round-robin should alternate, got %q twice", first.ID)
	}
}

func TestResolverCloseIdempotentAndGetFails(t *testing.T) {
	m := NewMemory()
	defer func() { _ = m.Close() }()
	ctx := context.Background()
	if err := m.Register(ctx, passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(m)
	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close must be idempotent, got %v", err)
	}
	if _, err := r.Get(ctx, "svc", Filter{}); !errors.Is(err, ErrResolverClosed) {
		t.Fatalf("Get after Close: want ErrResolverClosed, got %v", err)
	}
	if _, err := r.GetAll(ctx, "svc", Filter{}); !errors.Is(err, ErrResolverClosed) {
		t.Fatalf("GetAll after Close: want ErrResolverClosed, got %v", err)
	}
}

func TestEndpointOf(t *testing.T) {
	inst := passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"},
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"},
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8081"},
	)

	// 协议匹配：稳定顺序第一条。
	ep, err := EndpointOf(inst, ProtocolHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Address != "10.0.0.1:8080" {
		t.Fatalf("want first http endpoint, got %q", ep.Address)
	}
	ep, err = EndpointOf(inst, ProtocolGRPC)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Address != "10.0.0.1:9090" {
		t.Fatalf("want grpc endpoint, got %q", ep.Address)
	}

	// protocol 空：取 Endpoints[0]。
	ep, err = EndpointOf(inst, "")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Address != "10.0.0.1:8080" {
		t.Fatalf("want Endpoints[0], got %q", ep.Address)
	}

	// 无匹配 / 无 Endpoint：ErrNoInstance。
	if _, err := EndpointOf(inst, ProtocolHTTPS); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("no match: want ErrNoInstance, got %v", err)
	}
	if _, err := EndpointOf(passing("svc", "i2"), ""); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("no endpoints: want ErrNoInstance, got %v", err)
	}
}
