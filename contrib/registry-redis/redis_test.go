package registryredis

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/redis/go-redis/v9"
)

// newTestBackend 起一个 miniredis 并构造后端；返回裸客户端供断言键布局，
// 连接清理由 t.Cleanup 负责。
func newTestBackend(t *testing.T, opts ...Option) (*Client, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, opts...), rdb, mr
}

func inst(name, id string) registry.Instance {
	return registry.Instance{
		Name:      name,
		ID:        id,
		Version:   "v1",
		Status:    registry.StatusPassing,
		Endpoints: []registry.Endpoint{{Protocol: registry.ProtocolHTTP, Address: "127.0.0.1:8080"}},
	}
}

// nextWithin 以超时保护执行一次 Next（阻塞语义的失败保护）。
func nextWithin(t *testing.T, w registry.Watcher) ([]registry.Instance, error) {
	t.Helper()
	type result struct {
		insts []registry.Instance
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		insts, err := w.Next()
		ch <- result{insts, err}
	}()
	select {
	case r := <-ch:
		return r.insts, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Next() did not return within timeout")
		return nil, nil
	}
}

// waitFor 以 50ms 步进轮询断言条件成立（兜底 tick / 异步推送的同步点）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestBadInput：空服务名/实例 ID 在写路径三接口上统一返回 ErrBadName。
func TestBadInput(t *testing.T) {
	c, _, _ := newTestBackend(t)
	ctx := context.Background()
	if err := c.Register(ctx, registry.Instance{ID: "i1"}); !errors.Is(err, registry.ErrBadName) {
		t.Errorf("Register(no name) = %v", err)
	}
	if err := c.Register(ctx, registry.Instance{Name: "svc"}); !errors.Is(err, registry.ErrBadName) {
		t.Errorf("Register(no id) = %v", err)
	}
	if err := c.Deregister(ctx, "", "i1"); !errors.Is(err, registry.ErrBadName) {
		t.Errorf("Deregister(no name) = %v", err)
	}
	if err := c.Heartbeat(ctx, "svc", ""); !errors.Is(err, registry.ErrBadName) {
		t.Errorf("Heartbeat(no id) = %v", err)
	}
}

// TestRegisterUpsert：同 ID 重复注册是 last-write-wins upsert，目录始终一条。
func TestRegisterUpsert(t *testing.T) {
	c, _, _ := newTestBackend(t)
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}
	updated := inst("svc", "i1")
	updated.Endpoints = []registry.Endpoint{{Protocol: registry.ProtocolGRPC, Address: "10.0.0.1:9090"}}
	if err := c.Register(ctx, updated); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Endpoints[0].Protocol != registry.ProtocolGRPC {
		t.Fatalf("upsert = %+v", got)
	}
}

// TestTTLOfflineAndLazyCleanup：键过期即下线（GetService 不可见），索引
// 成员由同一次读惰性清除，不残留。
func TestTTLOfflineAndLazyCleanup(t *testing.T) {
	c, rdb, mr := newTestBackend(t, WithTTL(300*time.Millisecond))
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}

	mr.FastForward(500 * time.Millisecond)

	got, err := c.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("after expiry got %+v, want none", got)
	}
	members, err := rdb.SMembers(ctx, c.indexKey("svc")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("index after lazy cleanup = %v, want empty", members)
	}
}

// TestHeartbeatRenewAndExpired：Heartbeat 续命后实例在超过原注册窗口仍在线；
// 键过期后 Heartbeat 返回 ErrNotRegistered（Consul TTL check 失败同语义）。
func TestHeartbeatRenewAndExpired(t *testing.T) {
	c, _, mr := newTestBackend(t, WithTTL(2*time.Second))
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}

	mr.FastForward(1500 * time.Millisecond) // 距注册 1.5s，无续命必剩 0.5s
	if err := c.Heartbeat(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(1600 * time.Millisecond) // 距续命 1.6s、距注册 3.1s：续命后仍在线

	got, err := c.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after renew got %+v, want 1 instance", got)
	}

	mr.FastForward(600 * time.Millisecond) // 距续命 2.2s > ttl：过期
	if err := c.Heartbeat(ctx, "svc", "i1"); !errors.Is(err, registry.ErrNotRegistered) {
		t.Fatalf("heartbeat after expiry = %v, want ErrNotRegistered", err)
	}
}

// TestGetServiceFilter：Filter 语义（默认只留 Passing / 协议 / 标签全匹配）
// 由 MatchFilter 后置应用。
func TestGetServiceFilter(t *testing.T) {
	c, _, _ := newTestBackend(t)
	ctx := context.Background()

	web := inst("svc", "web")
	web.Tags = []string{"edge"}
	grpcOnly := inst("svc", "grpc")
	grpcOnly.Endpoints = []registry.Endpoint{{Protocol: registry.ProtocolGRPC, Address: "127.0.0.1:9090"}}
	sick := inst("svc", "sick")
	sick.Status = registry.StatusCritical
	for _, i := range []registry.Instance{web, grpcOnly, sick} {
		if err := c.Register(ctx, i); err != nil {
			t.Fatal(err)
		}
	}

	got, err := c.GetService(ctx, "svc", registry.Filter{})
	if err != nil || len(got) != 2 {
		t.Fatalf("default filter = %v (err %v), want 2 passing", got, err)
	}
	got, err = c.GetService(ctx, "svc", registry.Filter{Protocol: registry.ProtocolGRPC})
	if err != nil || len(got) != 1 || got[0].ID != "grpc" {
		t.Fatalf("grpc filter = %v (err %v)", got, err)
	}
	got, err = c.GetService(ctx, "svc", registry.Filter{Tags: []string{"edge"}})
	if err != nil || len(got) != 1 || got[0].ID != "web" {
		t.Fatalf("tag filter = %v (err %v)", got, err)
	}
	got, err = c.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil || len(got) != 3 {
		t.Fatalf("include unhealthy = %v (err %v), want 3", got, err)
	}
}

// TestWatchPushOnChange：Watch 走 Pub/Sub 推送——注册/注销后快照在下一个
// Next 即可见，无需等兜底 tick。
func TestWatchPushOnChange(t *testing.T) {
	c, _, _ := newTestBackend(t)
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}

	w, err := c.Watch(ctx, "svc", registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	snap, err := nextWithin(t, w)
	if err != nil || len(snap) != 1 {
		t.Fatalf("first Next = %v (err %v), want 1", snap, err)
	}

	if err := c.Register(ctx, inst("svc", "i2")); err != nil {
		t.Fatal(err)
	}
	snap, err = nextWithin(t, w)
	if err != nil || len(snap) != 2 {
		t.Fatalf("after register = %v (err %v), want 2", snap, err)
	}

	if err := c.Deregister(ctx, "svc", "i1"); err != nil {
		t.Fatal(err)
	}
	snap, err = nextWithin(t, w)
	if err != nil || len(snap) != 1 || snap[0].ID != "i2" {
		t.Fatalf("after deregister = %v (err %v), want [i2]", snap, err)
	}
}

// TestWatchFallbackCatchesExpiry：键过期不发 Pub/Sub 事件，兜底 tick 必须
// 让 Watcher 看到实例下线——这是 fallback 通道存在的根本理由。
func TestWatchFallbackCatchesExpiry(t *testing.T) {
	c, _, mr := newTestBackend(t,
		WithTTL(300*time.Millisecond),
		WithWatchFallbackInterval(100*time.Millisecond),
	)
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}

	w, err := c.Watch(ctx, "svc", registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	snap, err := nextWithin(t, w)
	if err != nil || len(snap) != 1 {
		t.Fatalf("first Next = %v (err %v), want 1", snap, err)
	}

	mr.FastForward(500 * time.Millisecond)

	waitFor(t, 3*time.Second, func() bool {
		snap, err := nextWithin(t, w)
		if err != nil {
			t.Fatalf("fallback Next: %v", err)
		}
		return len(snap) == 0
	}, "watcher did not observe expiry via fallback tick")
}

// TestWatchFilterApplied：会话核心对推送做 Filter 后置应用，订阅者只见
// 符合条件的实例。
func TestWatchFilterApplied(t *testing.T) {
	c, _, _ := newTestBackend(t)
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}

	w, err := c.Watch(ctx, "svc", registry.Filter{Protocol: registry.ProtocolGRPC})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	snap, err := nextWithin(t, w)
	if err != nil || len(snap) != 0 {
		t.Fatalf("filtered first Next = %v (err %v), want empty", snap, err)
	}

	peer := inst("svc", "i2")
	peer.Endpoints = []registry.Endpoint{{Protocol: registry.ProtocolGRPC, Address: "127.0.0.1:9090"}}
	if err := c.Register(ctx, peer); err != nil {
		t.Fatal(err)
	}
	snap, err = nextWithin(t, w)
	if err != nil || len(snap) != 1 || snap[0].ID != "i2" {
		t.Fatalf("after grpc register = %v (err %v), want [i2]", snap, err)
	}
}

// TestKeyPrefixIsolation：不同 key_prefix 的两套目录在同一 Redis 上互不可见。
func TestKeyPrefixIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	appA := New(rdb, WithKeyPrefix("app-a/"))
	appB := New(rdb, WithKeyPrefix("app-b/"))
	ctx := context.Background()
	if err := appA.Register(ctx, inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}

	got, err := appB.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil || len(got) != 0 {
		t.Fatalf("appB sees appA's instance: %v (err %v)", got, err)
	}
	got, err = appA.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("appA lost its instance: %v (err %v)", got, err)
	}
}

// TestCorruptInstanceSkippedAndEvicted：实例键内容损坏（非 JSON）时读路径
// 跳过该成员并从索引移出，其余实例不受影响。
func TestCorruptInstanceSkippedAndEvicted(t *testing.T) {
	c, rdb, _ := newTestBackend(t)
	ctx := context.Background()
	if err := c.Register(ctx, inst("svc", "good")); err != nil {
		t.Fatal(err)
	}
	// 直接写入损坏数据模拟半写/人工误操作。
	if err := rdb.Set(ctx, c.instanceKey("svc", "bad"), "{not-json", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, c.indexKey("svc"), "bad").Err(); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetService(ctx, "svc", registry.Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("got %+v, want only [good]", got)
	}
	members, err := rdb.SMembers(ctx, c.indexKey("svc")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(members, []string{"good"}) {
		t.Fatalf("index after eviction = %v, want [good]", members)
	}
}
