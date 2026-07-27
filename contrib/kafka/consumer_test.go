package kafka

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/segmentio/kafka-go"
)

func TestGetMessageID(t *testing.T) {
	withHeader := &kafka.Message{Headers: []kafka.Header{
		{Key: pubsub.MessageIDKey.String(), Value: []byte("id-123")},
	}}
	if got := GetMessageID(withHeader); got != "id-123" {
		t.Fatalf("expected id-123, got %q", got)
	}

	withoutHeader := &kafka.Message{}
	if got := GetMessageID(withoutHeader); got != "" {
		t.Fatalf("expected empty id, got %q", got)
	}
}

func TestNewMessage(t *testing.T) {
	t.Run("maps id, payload and headers to metadata", func(t *testing.T) {
		kmsg := kafka.Message{
			Value: []byte("payload"),
			Headers: []kafka.Header{
				{Key: pubsub.MessageIDKey.String(), Value: []byte("id-1")},
				{Key: "trace-id", Value: []byte("abc")},
			},
		}
		msg := NewMessage(kmsg)
		if msg.UUID != "id-1" {
			t.Fatalf("expected UUID id-1, got %q", msg.UUID)
		}
		if string(msg.Payload) != "payload" {
			t.Fatalf("unexpected payload: %q", msg.Payload)
		}
		if got := msg.Metadata.Get("trace-id"); got != "abc" {
			t.Fatalf("expected metadata trace-id=abc, got %q", got)
		}
	})

	t.Run("missing id header leaves UUID empty", func(t *testing.T) {
		msg := NewMessage(kafka.Message{Value: []byte("x")})
		if msg.UUID != "" {
			t.Fatalf("expected empty UUID, got %q", msg.UUID)
		}
	})
}

// newTestConsumer builds a Consumer with the fake reader/broker injected,
// mirroring what NewConsumer does without constructing a real kafka.Reader.
func newTestConsumer(reader messageReader, broker *fakeBroker, options ConsumerOptions) *Consumer {
	c := &Consumer{
		options:   options,
		eventName: "test-event",
		broker:    broker,
		reader:    reader,
	}
	c.ctx, c.closeCtx = context.WithCancel(context.Background())
	return c
}

func startConsumer(t *testing.T, c *Consumer) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Start(context.Background()) }()
	return done
}

func stopConsumer(t *testing.T, c *Consumer, done chan error) {
	t.Helper()
	c.Stop(context.Background())
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// Regression test for commit 31e1db2: Start must create the watermill message
// once and publish exactly one broker message per consumed kafka message,
// with the published UUID matching the message used for logging.
func TestConsumerStartPublishesEachKafkaMessageOnce(t *testing.T) {
	handler := newCaptureHandler(slog.LevelDebug, "recv kafka message")
	defer slogSwap(handler)()

	kmsgs := []fetchResult{
		{msg: kafka.Message{
			Value: []byte("with-id"),
			Headers: []kafka.Header{
				{Key: pubsub.MessageIDKey.String(), Value: []byte("id-1")},
			},
		}},
		{msg: kafka.Message{Value: []byte("without-id")}},
	}
	reader := newFakeReader(kmsgs...)
	broker := &fakeBroker{}
	consumer := newTestConsumer(reader, broker, ConsumerOptions{
		Topic:      "topic-a",
		Group:      "group-a",
		LogMessage: true,
	})

	done := startConsumer(t, consumer)
	waitUntil(t, 5*time.Second, func() bool {
		return len(broker.publishedMessages()) == 2 && reader.committedCount() == 2
	})
	stopConsumer(t, consumer, done)

	published := broker.publishedMessages()
	if len(published) != 2 {
		t.Fatalf("expected exactly 2 published messages (one per kafka message), got %d", len(published))
	}
	for i, p := range published {
		if p.topicName != "test-event" {
			t.Fatalf("message %d published to %q, want test-event", i, p.topicName)
		}
	}
	if string(published[0].msg.Payload) != "with-id" || string(published[1].msg.Payload) != "without-id" {
		t.Fatalf("unexpected payloads: %q, %q", published[0].msg.Payload, published[1].msg.Payload)
	}
	// The published message must carry the UUID derived from the kafka header.
	if published[0].msg.UUID != "id-1" {
		t.Fatalf("expected published UUID id-1, got %q", published[0].msg.UUID)
	}
	// The UUID logged on receive must be the one of the published message,
	// i.e. Start reuses a single NewMessage instance.
	loggedID, ok := handler.firstAttr("msg_id")
	if !ok {
		t.Fatal("expected a 'recv kafka message' log record with msg_id")
	}
	if loggedID != published[0].msg.UUID {
		t.Fatalf("logged msg_id %q does not match published UUID %q", loggedID, published[0].msg.UUID)
	}
}

func TestConsumerStartBrokerErrorSkipsCommit(t *testing.T) {
	reader := newFakeReader(fetchResult{msg: kafka.Message{Value: []byte("x"), Offset: 7}})
	broker := &fakeBroker{publishErr: errors.New("broker down")}
	consumer := newTestConsumer(reader, broker, ConsumerOptions{Topic: "t", Group: "g"})

	done := startConsumer(t, consumer)
	waitUntil(t, 5*time.Second, func() bool { return broker.publishCallCount() == 1 })
	// Give the loop a moment to (incorrectly) commit if it were going to.
	time.Sleep(50 * time.Millisecond)
	if got := reader.committedCount(); got != 0 {
		t.Fatalf("expected no commit on broker error, got %d commits", got)
	}
	stopConsumer(t, consumer, done)
}

func TestConsumerStartBrokerErrorStillCommitsWhenEnabled(t *testing.T) {
	reader := newFakeReader(fetchResult{msg: kafka.Message{Value: []byte("x"), Offset: 9}})
	broker := &fakeBroker{publishErr: errors.New("broker down")}
	consumer := newTestConsumer(reader, broker, ConsumerOptions{
		Topic:                    "t",
		Group:                    "g",
		StillCommitOnBrokerError: true,
	})

	done := startConsumer(t, consumer)
	waitUntil(t, 5*time.Second, func() bool { return reader.committedCount() == 1 })
	stopConsumer(t, consumer, done)
}

func TestConsumerStartFetchErrorInvokesErrorCallback(t *testing.T) {
	fetchErr := errors.New("fetch boom")
	reader := newFakeReader(
		fetchResult{err: fetchErr},
		fetchResult{msg: kafka.Message{Value: []byte("recovered")}},
	)
	broker := &fakeBroker{}

	var callbackCount atomic.Int32
	var lastErr atomic.Value
	consumer := newTestConsumer(reader, broker, ConsumerOptions{
		Topic: "t",
		Group: "g",
		ErrorCallbackFunc: func(err error) {
			callbackCount.Add(1)
			lastErr.Store(err)
		},
	})

	done := startConsumer(t, consumer)
	waitUntil(t, 5*time.Second, func() bool { return len(broker.publishedMessages()) == 1 })
	if callbackCount.Load() < 1 {
		t.Fatal("expected error callback to be invoked on fetch error")
	}
	if err, _ := lastErr.Load().(error); !errors.Is(err, fetchErr) {
		t.Fatalf("expected callback error %v, got %v", fetchErr, err)
	}
	stopConsumer(t, consumer, done)
}

func TestConsumerStopIsIdempotent(t *testing.T) {
	reader := newFakeReader()
	consumer := newTestConsumer(reader, &fakeBroker{}, ConsumerOptions{Topic: "t", Group: "g"})
	consumer.Stop(context.Background())
	consumer.Stop(context.Background()) // must not panic
}

func TestConsumerName(t *testing.T) {
	consumer := newTestConsumer(newFakeReader(), &fakeBroker{}, ConsumerOptions{Topic: "orders"})
	if got := consumer.Name(); got != "kafka-consumer-orders" {
		t.Fatalf("unexpected name: %q", got)
	}
}

func TestNewConsumer(t *testing.T) {
	c := NewConsumer("evt", &fakeBroker{}, ConsumerOptions{
		Brokers: []string{"localhost:9092"},
		Topic:   "topic-a",
		Group:   "group-a",
	})
	if c == nil {
		t.Fatal("expected non-nil consumer")
	}
	if _, ok := c.reader.(*kafka.Reader); !ok {
		t.Fatalf("expected reader to be *kafka.Reader, got %T", c.reader)
	}
	if got := c.Name(); got != "kafka-consumer-topic-a" {
		t.Fatalf("unexpected name: %q", got)
	}
	c.Stop(context.Background()) // closes the real reader without any broker interaction
}

func TestConsumerBuilder(t *testing.T) {
	builder := NewConsumerBuilder("evt", &fakeBroker{}, ConsumerOptions{
		Brokers:   []string{"localhost:9092"},
		Topic:     "topic-a",
		Group:     "group-a",
		Instances: 3,
	})
	if got := builder.Options().Instances; got != 3 {
		t.Fatalf("expected 3 instances, got %d", got)
	}
	component := builder.Build()
	if _, ok := component.(*Consumer); !ok {
		t.Fatalf("expected Build to return *Consumer, got %T", component)
	}
}
