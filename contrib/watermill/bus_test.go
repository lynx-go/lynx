package watermill_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

func TestWatermillSubscribeAfterStart(t *testing.T) {
	mem := watermill.NewMemoryTransport()
	bus := watermill.New(eventbus.Options{DefaultTransport: mem})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	got := make(chan string, 1)
	topic := eventbus.NewTopic[map[string]string]("order.created")
	if err := topic.Subscribe(context.Background(), "after-start", func(ctx context.Context, e *eventbus.Event[map[string]string]) error {
		got <- e.Payload["id"]
		return nil
	}, eventbus.WithBus(bus)); err != nil {
		t.Fatalf("Subscribe after Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := topic.Publish(context.Background(), map[string]string{"id": "1"}, eventbus.WithBus(bus)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case id := <-got:
		if id != "1" {
			t.Fatalf("got %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for event after dynamic Subscribe")
	}
	stopWithin(t, bus, 2*time.Second)
}

func TestWatermillBusStopAfterSubscribeCompletesQuickly(t *testing.T) {
	mem := watermill.NewMemoryTransport()
	bus := watermill.New(eventbus.Options{DefaultTransport: mem})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	if err := bus.Subscribe(context.Background(), "order.created", "stop-h", func(ctx context.Context, e *eventbus.RawEvent) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	stopWithin(t, bus, 2*time.Second)
}

func TestLifecycleForcedToMemory(t *testing.T) {
	mem := watermill.NewMemoryTransport()
	bus := watermill.New(eventbus.Options{DefaultTransport: mem})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	got := make(chan struct{}, 1)
	if err := eventbus.AppStartedTopic.Subscribe(context.Background(), "life", func(ctx context.Context, e *eventbus.Event[eventbus.AppEvent]) error {
		got <- struct{}{}
		return nil
	}, eventbus.WithBus(bus)); err != nil {
		t.Fatalf("Subscribe lynx.*: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := eventbus.AppStartedTopic.Publish(context.Background(), eventbus.AppEvent{Name: "t", Time: time.Now()}, eventbus.WithBus(bus)); err != nil {
		t.Fatalf("Publish lynx.*: %v", err)
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("lynx.* event not delivered via MemoryTransport")
	}
	stopWithin(t, bus, 2*time.Second)
}

func TestRouteLifecycleToNonMemoryFails(t *testing.T) {
	bus := watermill.New(eventbus.Options{})
	fake := &nonMemoryTransport{}
	err := bus.RouteKey(eventbus.TopicAppStarted, fake, "x")
	if err == nil {
		t.Fatal("want error routing lynx.* to non-memory")
	}
}

func TestInitRejectsLifecycleAutoRoute(t *testing.T) {
	fake := &nonMemoryTransport{topics: []string{eventbus.TopicAppStarted}}
	bus := watermill.New(eventbus.Options{Transports: []eventbus.Transport{fake}})
	if err := bus.Init(nil); err == nil {
		t.Fatal("want Init error for lynx.* on non-memory transport")
	}
}

type nonMemoryTransport struct {
	topics []string
}

func (t *nonMemoryTransport) Publish(ctx context.Context, topic string, e *eventbus.RawEvent) error {
	return errors.New("not memory")
}
func (t *nonMemoryTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan *eventbus.RawEvent, error) {
	return nil, errors.New("not memory")
}
func (t *nonMemoryTransport) Topics() []string { return t.topics }
func (t *nonMemoryTransport) Close() error     { return nil }

func stopWithin(t *testing.T, bus eventbus.Bus, d time.Duration) {
	t.Helper()
	start := time.Now()
	if err := bus.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v (elapsed %v)", err, time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > d {
		t.Fatalf("Stop took %v, want <= %v (subscriber Close must unblock router)", elapsed, d)
	}
}

func waitBus(t *testing.T, bus eventbus.Bus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bus.CheckHealth() == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bus not running")
}
