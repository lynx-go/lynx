package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/lynx-go/lynx/internal/clock"
)

// TestMemoryClaimTTLBoundary：ttl-ε 仍占用；恰好 ttl 过期后可重占。
// 假时钟确定性断言边界（此前靠 sleep 真实时间）。
func TestMemoryClaimTTLBoundary(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	c := NewMemory(WithClock(fc))
	ctx := context.Background()

	ok, err := c.Claim(ctx, "job", 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, _ = c.Claim(ctx, "job", 10*time.Second); ok {
		t.Fatal("second claim must lose while occupied")
	}

	fc.Advance(10*time.Second - time.Nanosecond)
	if ok, _ = c.Claim(ctx, "job", 10*time.Second); ok {
		t.Fatal("claim at ttl-ε must still be occupied")
	}

	fc.Advance(time.Nanosecond)
	if ok, _ = c.Claim(ctx, "job", 10*time.Second); !ok {
		t.Fatal("claim at exactly ttl must succeed after expiry")
	}
}

// TestMemoryAcquireRenewAtInterval：假时钟推进恰好一个续约间隔（ttl/3），
// 槽位到期时间被刷新（不 sleep 真实时间）。
func TestMemoryAcquireRenewAtInterval(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	c := NewMemory(WithClock(fc)).(*memory)

	lease, ok, err := c.Acquire(context.Background(), "leader", 9*time.Second) // 续约间隔 = 3s
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lease.Release(context.Background()) }()

	// 等待续约循环注册定时器，再推进恰好一个续约间隔。
	waitForTimers(t, fc, 1)
	fc.Advance(3 * time.Second)

	// 续约异步完成：轮询直到槽位 exp 刷新为「续约点 + ttl」。
	want := time.Unix(0, 0).Add(3*time.Second + 9*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		sl := c.slots[c.opts.key("leader")]
		var exp time.Time
		if sl != nil {
			exp = sl.exp
		}
		c.mu.Unlock()
		if exp.Equal(want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("slot expiry was not refreshed to %v", want)
}
