package eventbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResolveBusOrder(t *testing.T) {
	SetDefault(nil)
	defer SetDefault(nil)

	def := NewMemoryBus(Options{})
	ctxBus := NewMemoryBus(Options{})
	optBus := NewMemoryBus(Options{})

	t.Run("none", func(t *testing.T) {
		_, err := resolveBus(context.Background(), nil)
		if err == nil {
			t.Fatal("want error when no bus")
		}
	})

	t.Run("default", func(t *testing.T) {
		SetDefault(def)
		b, err := resolveBus(context.Background(), nil)
		if err != nil || b != def {
			t.Fatalf("got %v err=%v, want default", b, err)
		}
		SetDefault(nil)
	})

	t.Run("context over default", func(t *testing.T) {
		SetDefault(def)
		ctx := ContextWithBus(context.Background(), ctxBus)
		b, err := resolveBus(ctx, nil)
		if err != nil || b != ctxBus {
			t.Fatalf("got %v err=%v, want context bus", b, err)
		}
		SetDefault(nil)
	})

	t.Run("option over context", func(t *testing.T) {
		SetDefault(def)
		ctx := ContextWithBus(context.Background(), ctxBus)
		b, err := resolveBus(ctx, optBus)
		if err != nil || b != optBus {
			t.Fatalf("got %v err=%v, want option bus", b, err)
		}
		SetDefault(nil)
	})
}

func TestTopicPublishSubscribeViaContext(t *testing.T) {
	SetDefault(nil)
	defer SetDefault(nil)

	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	waitRunning(t, bus)

	appCtx := ContextWithBus(context.Background(), bus)
	topic := NewTopic[map[string]string]("api.ctx")
	got := make(chan string, 1)
	if err := topic.Subscribe(appCtx, "h-ctx", func(ctx context.Context, e *Event[map[string]string]) error {
		got <- e.Payload["id"]
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if err := topic.Publish(appCtx, map[string]string{"id": "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case id := <-got:
		if id != "1" {
			t.Fatalf("got %q, want 1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestTopicPublishViaDefault(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	waitRunning(t, bus)
	SetDefault(bus)
	defer SetDefault(nil)

	topic := NewTopic[map[string]string]("api.def")
	got := make(chan string, 1)
	if err := topic.Subscribe(context.Background(), "h-def", func(ctx context.Context, e *Event[map[string]string]) error {
		got <- e.Payload["id"]
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := topic.Publish(context.Background(), map[string]string{"id": "2"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case id := <-got:
		if id != "2" {
			t.Fatalf("got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestTopicWithBusOption(t *testing.T) {
	SetDefault(nil)
	defer SetDefault(nil)

	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	waitRunning(t, bus)

	other := NewMemoryBus(Options{})
	_ = other.Init(nil)
	octx, ocancel := context.WithCancel(context.Background())
	defer ocancel()
	go other.Start(octx)
	waitRunning(t, other)

	topic := NewTopic[map[string]string]("api.opt")
	got := make(chan string, 1)
	if err := topic.Subscribe(context.Background(), "h-opt", func(ctx context.Context, e *Event[map[string]string]) error {
		got <- e.Payload["id"]
		return nil
	}, WithBus(other)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// Publish to wrong bus (via context) must not deliver
	ctxWrong := ContextWithBus(context.Background(), bus)
	_ = topic.Publish(ctxWrong, map[string]string{"id": "wrong"})
	select {
	case <-got:
		t.Fatal("delivered to wrong bus")
	case <-time.After(50 * time.Millisecond):
	}

	if err := topic.Publish(context.Background(), map[string]string{"id": "ok"}, WithBus(other)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case id := <-got:
		if id != "ok" {
			t.Fatalf("got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestResolveBusErrorMessage(t *testing.T) {
	SetDefault(nil)
	_, err := resolveBus(context.Background(), nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, ErrNoBus) && err.Error() == "" {
		t.Fatalf("unexpected err: %v", err)
	}
}
