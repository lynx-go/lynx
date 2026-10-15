//go:build integration

// 集成测试对真实 Redis 做端到端冒烟（契约套件 + 真实 TTL 过期链路）：
//
//	LYNX_IT_REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./...
//
// 无本地 Redis 时 Skip（对齐 contrib/consul 的 integration_test 约定）。
package registryredis_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
	registryredis "github.com/lynx-go/lynx/contrib/registry-redis"
	"github.com/lynx-go/lynx/contrib/registry/registrytest"
	"github.com/redis/go-redis/v9"
)

// integrationRedis 返回可达的真实 Redis 客户端；不可达时 Skip。
func integrationRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("LYNX_IT_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no local redis at %s: %v", addr, err)
	}
	return rdb
}

// TestIntegrationConformance：契约三套件对真实 Redis 全量跑一遍。
func TestIntegrationConformance(t *testing.T) {
	rdb := integrationRedis(t)
	factory := func(t *testing.T) (registry.Registry, registry.Discovery) {
		// 每次调用独立前缀：套件内多次 factory 互不残留旧数据。
		prefix := fmt.Sprintf("it-%d/", time.Now().UnixNano())
		c := registryredis.New(rdb, registryredis.WithKeyPrefix(prefix))
		t.Cleanup(func() { _ = c.Close() })
		return c, c
	}
	registrytest.TestRegistry(t, factory)
	registrytest.TestDiscovery(t, factory)
	registrytest.TestWatcher(t, factory)
}

// TestIntegrationTTLExpiry：真实服务端的键过期链路——无心跳的实例在 TTL
// 后从 GetService 与 Watch 双通道下线。
func TestIntegrationTTLExpiry(t *testing.T) {
	rdb := integrationRedis(t)
	prefix := fmt.Sprintf("it-%d/", time.Now().UnixNano())
	c := registryredis.New(rdb,
		registryredis.WithKeyPrefix(prefix),
		registryredis.WithTTL(1*time.Second),
		registryredis.WithWatchFallbackInterval(300*time.Millisecond),
	)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	inst := registry.Instance{
		Name:      "redis-it",
		ID:        "i1",
		Status:    registry.StatusPassing,
		Endpoints: []registry.Endpoint{{Protocol: registry.ProtocolHTTP, Address: "127.0.0.1:18080"}},
	}
	if err := c.Register(ctx, inst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Deregister(context.Background(), inst.Name, inst.ID) })

	w, err := c.Watch(ctx, inst.Name, registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()

	if snap := nextIntegration(t, w); len(snap) != 1 {
		t.Fatalf("first snapshot = %v, want 1", snap)
	}
	if got, err := c.GetService(ctx, inst.Name, registry.Filter{}); err != nil || len(got) != 1 {
		t.Fatalf("GetService = %v (err %v)", got, err)
	}

	// 越过 TTL（无心跳）：GetService 立即下线，Watch 由兜底 tick 收敛。
	time.Sleep(1500 * time.Millisecond)
	if got, err := c.GetService(ctx, inst.Name, registry.Filter{}); err != nil || len(got) != 0 {
		t.Fatalf("GetService after ttl = %v (err %v), want none", got, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap := nextIntegration(t, w); len(snap) == 0 {
			return // Watcher 观察到下线，链路闭环
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("watcher did not observe expiry within deadline")
}

// nextIntegration 以超时保护执行一次 Next。
func nextIntegration(t *testing.T, w registry.Watcher) []registry.Instance {
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
		if r.err != nil {
			t.Fatalf("Next: %v", r.err)
		}
		return r.insts
	case <-time.After(5 * time.Second):
		t.Fatal("Next() did not return within timeout")
		return nil
	}
}
