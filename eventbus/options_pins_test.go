package eventbus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errPinAlways = errors.New("pin: always fails")

// TestWithRetryOptionAffectsSubscription：WithRetry 经 NewMemoryBus 生效
// （全局重试默认参与解析）——此前该构造器在仓库内无调用者。
func TestWithRetryOptionAffectsSubscription(t *testing.T) {
	opts := Options{}
	WithRetry(RetryOptions{MaxRetries: 1, Backoff: time.Millisecond})(&opts)
	bus := NewMemoryBus(opts)
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	var calls atomic.Int32
	if err := bus.Subscribe(context.Background(), "pin.retry", func(context.Context, *RawEvent) error {
		calls.Add(1)
		return errPinAlways
	}, WithHandlerName("pin-retry")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := bus.Publish(context.Background(), "pin.retry", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (1 + MaxRetries=1)", got)
	}
}

// TestWithMetadataOptionsReachHeaders：WithMetadata 整表 + WithMetadataField
// 单键进入 wire headers；且 WithMetadata 克隆调用方 map（后续单键写入不
// 反向污染调用方传入的映射）。
func TestWithMetadataOptionsReachHeaders(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	got := make(chan map[string]string, 1)
	if err := bus.Subscribe(context.Background(), "pin.meta", func(_ context.Context, e *RawEvent) error {
		got <- e.Headers
		return nil
	}, WithHandlerName("pin-meta")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	md := map[string]string{"a": "1"}
	if err := bus.Publish(context.Background(), "pin.meta", map[string]string{"k": "v"},
		WithMetadata(md), WithMetadataField("b", "2")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case headers := <-got:
		if headers["a"] != "1" || headers["b"] != "2" {
			t.Fatalf("headers = %v, want a=1 b=2", headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	if len(md) != 1 || md["a"] != "1" {
		t.Fatalf("caller map mutated: %v (WithMetadata must clone)", md)
	}
}
