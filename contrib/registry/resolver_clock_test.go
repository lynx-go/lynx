package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynx-go/lynx/internal/clock"
)

// TestResolverStaleBoundary 锁定 stale 判定的精确边界（假时钟，不再 sleep）：
// 年龄恰等于 staleMaxAge 仍有效；越过即丢弃并返回 ErrNoInstance。
func TestResolverStaleBoundary(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	fd := &fakeDiscovery{snap: []Instance{inst("a")}, silent: true}
	r := NewResolver(fd,
		WithResolverClock(fc),
		WithStaleMaxAge(10*time.Second),
		WithPollInterval(time.Hour))
	defer func() { _ = r.Close() }()

	e, ok := r.entryFor("svc")
	if !ok {
		t.Fatal("entryFor failed")
	}
	e.store([]Instance{inst("a")})
	ctx := context.Background()

	// 恰在 staleMaxAge：仍可读。
	fc.Advance(10 * time.Second)
	if _, err := r.Get(ctx, "svc", Filter{}); err != nil {
		t.Fatalf("at stale boundary Get = %v, want valid snapshot", err)
	}

	// 越过一个纳秒：丢弃并按未命中处理。
	fc.Advance(time.Nanosecond)
	if _, err := r.Get(ctx, "svc", Filter{}); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("past stale boundary Get = %v, want ErrNoInstance", err)
	}
}
