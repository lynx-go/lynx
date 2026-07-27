package kafka

import (
	"context"
	"testing"
	"time"
)

func TestBinderName(t *testing.T) {
	b := NewBinder(BinderOptions{})
	if got := b.Name(); got != "kafka-binder" {
		t.Fatalf("unexpected name: %q", got)
	}
}

func TestEventNameMappings(t *testing.T) {
	if got := ToProducerName("evt"); got != "evt:kafka-producer" {
		t.Fatalf("unexpected producer name: %q", got)
	}
	if got := ToConsumerName("evt"); got != "evt:kafka-consumer" {
		t.Fatalf("unexpected consumer name: %q", got)
	}
}

func TestNewBinderEventMappings(t *testing.T) {
	b := NewBinder(BinderOptions{
		SubscribeOptions: map[string]ConsumerOptions{
			"sub-1": {MappedEvent: "evt-in"},
		},
		PublishOptions: map[string]ProducerOptions{
			"pub-1": {MappedEvent: "evt-out"},
		},
	})

	topic, ok := b.CanSubscribe("evt-in")
	if !ok || topic != "evt-in:kafka-consumer" {
		t.Fatalf("CanSubscribe(evt-in) = %q, %v", topic, ok)
	}
	if _, ok := b.CanSubscribe("unknown"); ok {
		t.Fatal("CanSubscribe(unknown) should be false")
	}

	topic, ok = b.CanPublish("evt-out")
	if !ok || topic != "evt-out:kafka-producer" {
		t.Fatalf("CanPublish(evt-out) = %q, %v", topic, ok)
	}
	if _, ok := b.CanPublish("unknown"); ok {
		t.Fatal("CanPublish(unknown) should be false")
	}
}

func TestBinderCheckHealthBeforeStart(t *testing.T) {
	b := NewBinder(BinderOptions{})
	if err := b.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth to fail before Start")
	}
}

func TestBinderLifecycle(t *testing.T) {
	broker := &fakeBroker{}
	b := NewBinder(BinderOptions{
		SubscribeOptions: map[string]ConsumerOptions{
			"sub-1": {
				Brokers:     []string{"localhost:9092"},
				Topic:       "topic-in",
				Group:       "group-1",
				MappedEvent: "evt-in",
				Instances:   2,
			},
		},
		PublishOptions: map[string]ProducerOptions{
			"pub-1": {
				Brokers:     []string{"localhost:9092"},
				Topic:       "topic-out",
				MappedEvent: "evt-out",
			},
		},
	})
	b.SetBroker(broker)

	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	builders := b.ConsumerBuilders()
	if len(builders) != 1 {
		t.Fatalf("expected 1 consumer builder, got %d", len(builders))
	}
	if got := builders[0].Options().Instances; got != 2 {
		t.Fatalf("expected 2 instances, got %d", got)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- b.Start(context.Background()) }()

	waitUntil(t, 5*time.Second, func() bool { return broker.subscribeCalls() == 1 })
	sub := broker.subscriptions[0]
	if sub.topicName != "evt-out:kafka-producer" {
		t.Fatalf("expected subscription to evt-out:kafka-producer, got %q", sub.topicName)
	}
	if sub.handlerName != "pub-1-ProducerHandler" {
		t.Fatalf("unexpected handler name: %q", sub.handlerName)
	}

	if err := b.CheckHealth(); err != nil {
		t.Fatalf("expected CheckHealth to pass while running: %v", err)
	}

	b.Stop(context.Background())
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	if err := b.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth to fail after Stop")
	}
}
