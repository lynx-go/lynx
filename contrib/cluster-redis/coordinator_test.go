package clusterredis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/lynx-go/lynx/contrib/cluster"
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

func TestAcquireRenews(t *testing.T) {
	s, mr := newTestCoordinator(t)
	lease, ok, err := s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release(context.Background()) }()
	mr.FastForward(80 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)     // ttl/3 ≈ 66ms，给续约一次机会
	mr.FastForward(150 * time.Millisecond) // 80+150>200，无续约会过期
	_, ok, err = s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("lease should still be held after ttl due to renew")
	}
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
