package eventbus_test

import (
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

func TestTopicOptionsIsExported(t *testing.T) {
	topic := eventbus.NewTopic[string]("orders",
		eventbus.WithTopicMaxInFlight(3),
		eventbus.WithTopicHandlerTimeout(2*time.Second),
		eventbus.WithTopicAutoAck(),
		eventbus.WithTopicContinueOnError(),
		eventbus.WithTopicMarshaler(eventbus.JSONMarshaler{}),
	)
	opts := topic.Options()
	var _ = opts
	if opts.MaxInFlight != 3 {
		t.Fatalf("MaxInFlight = %d, want 3", opts.MaxInFlight)
	}
	if opts.HandlerTimeout != 2*time.Second {
		t.Fatalf("HandlerTimeout = %v, want 2s", opts.HandlerTimeout)
	}
	if !opts.AutoAck || !opts.ContinueOnError {
		t.Fatal("AutoAck/ContinueOnError not set")
	}
	if opts.Marshaler == nil {
		t.Fatal("Marshaler nil")
	}
	if topic.Name() != "orders" {
		t.Fatalf("Name = %q", topic.Name())
	}
}
