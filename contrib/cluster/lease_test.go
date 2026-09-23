package cluster

import (
	"context"
	"errors"
	"testing"
	"time"
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
// 引擎必须 cancel 租约 ctx（Lease.Context 随之取消）并退出。
func TestRunRenewLoopCancelsOnRenewError(t *testing.T) {
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	defer leaseCancel()

	calls := 0
	renew := func() error {
		calls++
		if calls >= 2 {
			return errors.New("lease lost")
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRenewLoop(leaseCtx, leaseCancel, time.Millisecond, renew)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("renew loop did not exit after renew error")
	}
	if calls < 2 {
		t.Fatalf("renew called %d times, want >= 2", calls)
	}
	select {
	case <-leaseCtx.Done():
	default:
		t.Fatal("engine must cancel lease ctx on renew error")
	}
}
