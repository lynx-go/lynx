package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/internal/clock"
)

func TestValidateCall(t *testing.T) {
	if err := ValidateCall(context.Background(), "x", time.Second); err != nil {
		t.Fatalf("valid call: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateCall(ctx, "x", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v", err)
	}
	if err := ValidateCall(context.Background(), "", time.Second); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("empty name: %v", err)
	}
	if err := ValidateCall(context.Background(), "x", 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("zero ttl: %v", err)
	}
}

func TestRenewInterval(t *testing.T) {
	if got := RenewInterval(30 * time.Second); got != 10*time.Second {
		t.Fatalf("30s → %s, want 10s", got)
	}
	if got := RenewInterval(time.Millisecond); got != time.Millisecond {
		t.Fatalf("1ms → %s, want 1ms floor", got)
	}
}

type minTTLFake struct{ Coordinator }

func (minTTLFake) MinTTL() time.Duration { return 42 * time.Second }

func TestMinTTLCapability(t *testing.T) {
	if got := MinTTL(NewMemory()); got != 0 {
		t.Fatalf("memory (no TTLAware) MinTTL = %s, want 0", got)
	}
	if got := MinTTL(minTTLFake{NewMemory()}); got != 42*time.Second {
		t.Fatalf("TTLAware MinTTL = %s, want 42s", got)
	}
}

// TestRunRenewLoopCancelsOnRenewError 钉住引擎契约：renew 首次返回错误时
// 引擎必须 cancel 租约 ctx（Lease.Context 随之取消）并退出。假时钟确定性
// 驱动——不再 sleep 真实时间。
func TestRunRenewLoopCancelsOnRenewError(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	defer leaseCancel()

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRenewLoop(leaseCtx, leaseCancel, time.Second, fc, func() error {
			calls.Add(1)
			return errors.New("lease lost")
		})
	}()

	waitForTimers(t, fc, 1)
	fc.Advance(time.Second)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("renew loop did not exit after renew error")
	}
	if calls.Load() != 1 {
		t.Fatalf("renew called %d times, want exactly 1 before exit", calls.Load())
	}
	select {
	case <-leaseCtx.Done():
	default:
		t.Fatal("engine must cancel lease ctx on renew error")
	}
}

// TestRunRenewLoopRenewsAtInterval：恰好 interval 触发一次；未到期不触发。
func TestRunRenewLoopRenewsAtInterval(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	go RunRenewLoop(ctx, cancel, time.Second, fc, func() error {
		calls.Add(1)
		return nil
	})
	waitForTimers(t, fc, 1)

	fc.Advance(time.Second - time.Nanosecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("renew called %d times before interval, want 0", got)
	}
	fc.Advance(time.Nanosecond)
	waitForCalls(t, &calls, 1)
	fc.Advance(time.Second)
	waitForCalls(t, &calls, 2)
}

// waitForTimers 等待假时钟上注册了 n 个定时器（loop goroutine 调度同步）。
func waitForTimers(t *testing.T, fc *clock.Fake, n int) {
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

// waitForCalls 等待续约计数达到 n。
func waitForCalls(t *testing.T, calls *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("renew calls = %d, want >= %d", calls.Load(), n)
}
