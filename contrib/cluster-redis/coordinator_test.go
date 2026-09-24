package clusterredis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/lynx-go/lynx/contrib/cluster"
	"github.com/lynx-go/lynx/internal/clock"
	"github.com/redis/go-redis/v9"
)

func newTestCoordinator(t *testing.T, opts ...cluster.Option) (cluster.Coordinator, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewCoordinator(rdb, opts...), mr
}

func TestClaimExclusiveAndExpiry(t *testing.T) {
	s, mr := newTestCoordinator(t, cluster.WithNamespace("app"))
	won, err := s.Claim(context.Background(), "job", 50*time.Millisecond)
	if err != nil || !won {
		t.Fatalf("first: won=%v err=%v", won, err)
	}
	won, err = s.Claim(context.Background(), "job", 50*time.Millisecond)
	if err != nil || won {
		t.Fatalf("second: won=%v err=%v", won, err)
	}
	mr.FastForward(80 * time.Millisecond)
	won, err = s.Claim(context.Background(), "job", 50*time.Millisecond)
	if err != nil || !won {
		t.Fatalf("after expiry: won=%v err=%v", won, err)
	}
}

func TestAcquireRelease(t *testing.T) {
	s, _ := newTestCoordinator(t)
	lease, ok, err := s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	_, ok, err = s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || ok {
		t.Fatalf("second acquire: ok=%v err=%v", ok, err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, ok, err = s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("after release: ok=%v err=%v", ok, err)
	}
}

// TestAcquireRenews：续约循环经假时钟确定性驱动——推进恰好一个续约窗口
// 后必须观察到 Redis TTL 被刷新；随后推进超过原 ttl，租约仍被持有。
func TestAcquireRenews(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	s, mr := newTestCoordinator(t, cluster.WithClock(fc))
	lease, ok, err := s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release(context.Background()) }()

	key := cluster.FormatKey("leader", cluster.WithClock(fc))
	// 等待续约循环注册定时器，再推进 ttl/3 ≈ 66ms 触发一次续约。
	waitForRedisTimers(t, fc, 1)
	fc.Advance(80 * time.Millisecond)
	mr.FastForward(80 * time.Millisecond)

	// 续约经 Lua 脚本异步刷新 TTL：轮询直到观察到刷新效果。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mr.TTL(key) > 150*time.Millisecond {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if ttl := mr.TTL(key); ttl <= 150*time.Millisecond {
		t.Fatalf("renew did not refresh TTL, ttl = %v", ttl)
	}

	// 总时间 80+150=230ms 已超过原 ttl=200ms：无续约必过期，续约后仍持有。
	mr.FastForward(150 * time.Millisecond)
	_, ok, err = s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("lease should still be held after ttl due to renew")
	}
}

// waitForRedisTimers 等待假时钟注册续约定时器（loop goroutine 调度同步）。
func waitForRedisTimers(t *testing.T, fc *clock.Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fc.TimerCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timers = %d, want >= %d", fc.TimerCount(), n)
}

func TestBadInput(t *testing.T) {
	s, _ := newTestCoordinator(t)
	if _, err := s.Claim(context.Background(), "", time.Second); !errors.Is(err, cluster.ErrEmptyName) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Claim(context.Background(), "x", 0); !errors.Is(err, cluster.ErrInvalidTTL) {
		t.Fatalf("got %v", err)
	}
}
