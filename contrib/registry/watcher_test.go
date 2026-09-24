package registry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// TestWatcherCoreFirstCalledOnce：first 只在首次 Next 调用一次，后续
// Next 走推送路径。
func TestWatcherCoreFirstCalledOnce(t *testing.T) {
	c := NewWatcherCore[string](context.Background(), nil)
	var calls atomic.Int32
	first := func() ([]string, error) {
		calls.Add(1)
		return []string{"first"}, nil
	}
	if got, err := c.Next(first); err != nil || len(got) != 1 || got[0] != "first" {
		t.Fatalf("first Next = %v, %v", got, err)
	}
	c.Push([]string{"second"})
	got, err := c.Next(first)
	if err != nil || len(got) != 1 || got[0] != "second" {
		t.Fatalf("second Next = %v, %v", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("first called %d times, want 1", calls.Load())
	}
}

// TestWatcherCorePushCoalesces：推送缓冲 1 最新替换——连续推送不消费时
// 只保留最新值。
func TestWatcherCorePushCoalesces(t *testing.T) {
	c := NewWatcherCore[string](context.Background(), nil)
	c.Push([]string{"old"})
	c.Push([]string{"new"})
	got, err := c.Next(nil)
	if err != nil || len(got) != 1 || got[0] != "new" {
		t.Fatalf("Next = %v, %v, want [new]", got, err)
	}
}

// TestWatcherCoreStopIdempotent：Stop 幂等、onStop 恰好一次、之后 Next
// 返回 ErrWatcherStopped。
func TestWatcherCoreStopIdempotent(t *testing.T) {
	var stops atomic.Int32
	c := NewWatcherCore[string](context.Background(), func() { stops.Add(1) })
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if stops.Load() != 1 {
		t.Fatalf("onStop called %d times, want 1", stops.Load())
	}
	if _, err := c.Next(nil); !errors.Is(err, ErrWatcherStopped) {
		t.Fatalf("Next after Stop = %v, want ErrWatcherStopped", err)
	}
}

// TestWatcherCoreCtxCancel：ctx 取消让阻塞中的 Next 返回 ctx.Err()。
func TestWatcherCoreCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := NewWatcherCore[string](ctx, nil)
	cancel()
	if _, err := c.Next(nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next = %v, want context.Canceled", err)
	}
}

// TestWatcherCoreDrainAndReceive：Drain 排空未消费推送；Receive 等待下一次。
func TestWatcherCoreDrainAndReceive(t *testing.T) {
	c := NewWatcherCore[string](context.Background(), nil)
	c.Push([]string{"stale"})
	c.Drain()
	c.Push([]string{"fresh"})
	got, err := c.Receive()
	if err != nil || len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("Receive = %v, %v, want [fresh]", got, err)
	}
}
