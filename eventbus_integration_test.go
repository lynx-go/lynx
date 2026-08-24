package lynx

import (
	"context"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

type busProbeService struct {
	received chan string
}

func (s *busProbeService) Name() string { return "probe" }
func (s *busProbeService) Init(ctx AppContext) error {
	// Subscribe in Init via Bus
	return ctx.Bus().Subscribe(ctx.Context(), "test.event", "probe-handler", func(ctx context.Context, e *eventbus.RawEvent) error {
		s.received <- string(e.Payload)
		return nil
	})
}
func (s *busProbeService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *busProbeService) Stop(ctx context.Context) error  { return nil }

func TestAppBusAvailableInInit(t *testing.T) {
	probe := &busProbeService{received: make(chan string, 1)}
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx: %v", err)
	}
	if app.Bus() == nil {
		t.Fatal("Bus() is nil, want memory bus")
	}
	app.Register(probe)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	// Wait for bus and service to start
	time.Sleep(100 * time.Millisecond)

	if err := app.Bus().Publish(context.Background(), "test.event", []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case got := <-probe.received:
		if got != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not receive event via App.Bus()")
	}
	app.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

type Order struct {
	ID string `json:"id"`
}

func TestBusPublishTypedViaApp(t *testing.T) {
	topic := eventbus.NewTopic[Order]("order.created")
	received := make(chan string, 1)
	app, _ := newLynx(NewOptions())
	app.Register(&busTypedProbe{topic: topic, received: received})
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	time.Sleep(100 * time.Millisecond)
	_ = eventbus.PublishTyped(context.Background(), app.Bus(), topic, Order{ID: "123"})
	select {
	case got := <-received:
		if got != "123" {
			t.Errorf("got %q, want 123", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("typed probe not received")
	}
	app.Close()
	<-done
}

type busTypedProbe struct {
	topic    eventbus.Topic[Order]
	received chan string
}

func (s *busTypedProbe) Name() string { return "typed-probe" }
func (s *busTypedProbe) Init(ctx AppContext) error {
	return eventbus.SubscribeTyped(ctx.Context(), ctx.Bus(), s.topic, "typed-handler", func(ctx context.Context, e *eventbus.Event[Order]) error {
		s.received <- e.Payload.ID
		return nil
	})
}
func (s *busTypedProbe) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *busTypedProbe) Stop(ctx context.Context) error  { return nil }
