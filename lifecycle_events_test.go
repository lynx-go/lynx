package lynx

import (
	"context"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// testService is a simple service for lifecycle event testing.
type testLifecycleService struct {
	name string
}

func (s *testLifecycleService) Name() string { return s.name }
func (s *testLifecycleService) Init(ctx AppContext) error { return nil }
func (s *testLifecycleService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *testLifecycleService) Stop(ctx context.Context) error { return nil }

func TestLifecycleEventsServiceRegistered(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx: %v", err)
	}
	received := make(chan string, 10)
	// Subscribe before Register, so we capture the event.
	if err := app.Bus().Subscribe(context.Background(), eventbus.TopicServiceRegistered, func(ctx context.Context, e *eventbus.RawEvent) error {
		// Decode ServiceEvent
		var se eventbus.ServiceEvent
		m := app.Bus().MarshalerFor(eventbus.TopicServiceRegistered)
		// Use generic helper to decode
		raw := &eventbus.RawEvent{Payload: e.Payload, Headers: e.Headers, ID: e.ID, Topic: e.Topic, Key: e.Key, Time: e.Time}
		// Instead of using DecodeTyped, just unmarshal directly
		if err := m.Unmarshal(e.Payload, &se); err == nil {
			received <- se.Service
		}
		_ = raw
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Register a service, should publish lynx.service.registered
	app.Register(&testLifecycleService{name: "my-service"})
	select {
	case svc := <-received:
		if svc != "my-service" {
			t.Errorf("got service %q, want my-service", svc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive service.registered event")
	}
}

func TestLifecycleEventsAppAndService(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx: %v", err)
	}
	// Channels for events
	appStarting := make(chan string, 1)
	appStarted := make(chan string, 1)
	serviceStarting := make(chan string, 1)
	serviceStarted := make(chan string, 1)
	serviceStopping := make(chan string, 1)
	serviceStopped := make(chan string, 1)

	bus := app.Bus()
	_ = bus.Subscribe(context.Background(), eventbus.TopicAppStarting, func(ctx context.Context, e *eventbus.RawEvent) error {
		appStarting <- e.Topic
		return nil
	})
	_ = bus.Subscribe(context.Background(), eventbus.TopicAppStarted, func(ctx context.Context, e *eventbus.RawEvent) error {
		appStarted <- e.Topic
		return nil
	})
	_ = bus.Subscribe(context.Background(), eventbus.TopicServiceStarting, func(ctx context.Context, e *eventbus.RawEvent) error {
		var se eventbus.ServiceEvent
		_ = bus.MarshalerFor(eventbus.TopicServiceStarting).Unmarshal(e.Payload, &se)
		if se.Service == "lifecycle-svc" {
			serviceStarting <- se.Service
		}
		return nil
	})
	_ = bus.Subscribe(context.Background(), eventbus.TopicServiceStarted, func(ctx context.Context, e *eventbus.RawEvent) error {
		var se eventbus.ServiceEvent
		_ = bus.MarshalerFor(eventbus.TopicServiceStarted).Unmarshal(e.Payload, &se)
		if se.Service == "lifecycle-svc" {
			serviceStarted <- se.Service
		}
		return nil
	})
	_ = bus.Subscribe(context.Background(), eventbus.TopicServiceStopping, func(ctx context.Context, e *eventbus.RawEvent) error {
		var se eventbus.ServiceEvent
		_ = bus.MarshalerFor(eventbus.TopicServiceStopping).Unmarshal(e.Payload, &se)
		if se.Service == "lifecycle-svc" {
			serviceStopping <- se.Service
		}
		return nil
	})
	_ = bus.Subscribe(context.Background(), eventbus.TopicServiceStopped, func(ctx context.Context, e *eventbus.RawEvent) error {
		var se eventbus.ServiceEvent
		_ = bus.MarshalerFor(eventbus.TopicServiceStopped).Unmarshal(e.Payload, &se)
		if se.Service == "lifecycle-svc" {
			serviceStopped <- se.Service
		}
		return nil
	})

	app.Register(&testLifecycleService{name: "lifecycle-svc"})

	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	// Wait for app starting/started and service starting/started
	select {
	case <-appStarting:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive app.starting")
	}
	select {
	case <-appStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive app.started")
	}
	select {
	case <-serviceStarting:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive service.starting")
	}
	select {
	case <-serviceStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive service.started")
	}

	// Shutdown
	app.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Close")
	}

	// After shutdown, we should have received stopping/stopped (allow a bit of time)
	select {
	case <-serviceStopping:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive service.stopping")
	}
	select {
	case <-serviceStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive service.stopped")
	}
}
