package eventbus_test

import (
	"testing"

	"github.com/lynx-go/lynx/eventbus"
)

func TestTopicOptionsIsExported(t *testing.T) {
	topic := eventbus.NewTopic[string]("orders",
		eventbus.WithTopicGroup("g1"),
		eventbus.WithTopicInstances(2),
		eventbus.WithTopicAutoAck(),
		eventbus.WithTopicContinueOnError(),
		eventbus.WithTopicMarshaler(eventbus.JSONMarshaler{}),
	)
	opts := topic.Options()
	var _ eventbus.TopicOptions = opts
	if opts.Group != "g1" {
		t.Fatalf("Group = %q, want g1", opts.Group)
	}
	if opts.Instances != 2 {
		t.Fatalf("Instances = %d, want 2", opts.Instances)
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
