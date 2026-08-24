package eventbus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryBusPublishSubscribe(t *testing.T) {
	b := NewMemoryBus(Options{})
	// Init with nil is allowed
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Start(ctx) }()

	// Wait for running
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := b.CheckHealth(); err != nil {
		t.Fatalf("bus not running: %v", err)
	}

	received := make(chan *RawEvent, 1)
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		received <- e
		return nil
	}, WithHandlerName("h1")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Give loop time to start
	time.Sleep(50 * time.Millisecond)

	if err := b.Publish(context.Background(), "order.created", map[string]string{"id": "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case e := <-received:
		if e.Topic != "order.created" {
			t.Errorf("topic = %q, want order.created", e.Topic)
		}
		if string(e.Payload) == "" {
			t.Error("payload empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive event")
	}

	// Test Typed helper
	topic := NewTopic[map[string]string]("order.typed")
	typedReceived := make(chan *Event[map[string]string], 1)
	if err := SubscribeTyped(context.Background(), b, topic, func(ctx context.Context, e *Event[map[string]string]) error {
		typedReceived <- e
		return nil
	}, WithHandlerName("h2")); err != nil {
		t.Fatalf("SubscribeTyped: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := PublishTyped(context.Background(), b, topic, map[string]string{"id": "2"}); err != nil {
		t.Fatalf("PublishTyped: %v", err)
	}
	select {
	case e := <-typedReceived:
		if e.Payload["id"] != "2" {
			t.Errorf("payload id = %q, want 2", e.Payload["id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive typed event")
	}

	// Dynamic subscribe after Start
	var count atomic.Int32
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		count.Add(1)
		return nil
	}, WithHandlerName("h3")); err != nil {
		t.Fatalf("dynamic Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	_ = b.Publish(context.Background(), "order.created", []byte("hello"))
	time.Sleep(100 * time.Millisecond)
	if count.Load() == 0 {
		t.Error("dynamic subscriber did not receive")
	}

	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSubscribeHandlerNameDefaultsToTopic(t *testing.T) {
	b := NewMemoryBus(Options{})
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Start(ctx) }()
	for i := 0; i < 50; i++ {
		if b.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := make(chan string, 1)
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		got <- e.Topic
		return nil
	}); err != nil {
		t.Fatalf("Subscribe without handler name: %v", err)
	}

	// 同 topic 再订一次且不指定 handlerName → 默认同名，应冲突
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		return nil
	}); err == nil {
		t.Fatal("expected duplicate handler name error")
	}

	// 显式命名可并存
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		return nil
	}, WithHandlerName("audit")); err != nil {
		t.Fatalf("Subscribe WithHandlerName: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	_ = b.Publish(context.Background(), "order.created", map[string]string{"id": "1"})
	select {
	case topic := <-got:
		if topic != "order.created" {
			t.Fatalf("topic = %q", topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive")
	}
	_ = b.Stop(context.Background())
}

func TestMemoryBusWithAppContext(t *testing.T) {
	b := NewMemoryBus(Options{BufferSize: 10})
	// Test Bus via lynx App
	// Use helper to ensure Bus works with Init
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init nil: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Start(ctx)
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{}, 1)
	_ = b.Subscribe(context.Background(), "test", func(ctx context.Context, e *RawEvent) error {
		done <- struct{}{}
		return nil
	})
	time.Sleep(20 * time.Millisecond)
	_ = b.Publish(context.Background(), "test", "payload")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscribe failed")
	}
	_ = b.Stop(context.Background())
}
