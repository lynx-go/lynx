package pubsub

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
)

func TestMemoryTransportPublishSubscribe(t *testing.T) {
	tp := NewMemoryTransport()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan *message.Message, 1)
	ch, err := tp.Subscribe(ctx, "test.event", SubscriptionOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go func() {
		for msg := range ch {
			received <- msg
		}
	}()

	msg := message.NewMessage("id-1", []byte("payload"))
	if err := tp.Publish(context.Background(), "test.event", msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-received:
		if got.UUID != "id-1" || string(got.Payload) != "payload" {
			t.Fatalf("unexpected message: %+v", got)
		}
		got.Ack()
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive message within 5s")
	}
}

func TestMemoryTransportLifecycle(t *testing.T) {
	tp := NewMemoryTransport()
	if err := tp.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error before Start")
	}
	if err := tp.Init(&fakeApp{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	startCtx, startCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tp.Start(startCtx) }()

	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return tp.CheckHealth() == nil }) {
		t.Fatal("transport did not become healthy")
	}

	startCancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

var _ Transport = (*MemoryTransport)(nil)
var _ lynx.Component = (*MemoryTransport)(nil)
