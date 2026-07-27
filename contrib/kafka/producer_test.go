package kafka

import (
	"context"
	"errors"
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/segmentio/kafka-go"
)

func TestNewProducerDefaultsWriterConfig(t *testing.T) {
	p := NewProducer(ProducerOptions{
		Brokers: []string{"localhost:9092"},
		Topic:   "topic-a",
	})
	if p == nil {
		t.Fatal("expected non-nil producer")
	}
	if _, ok := p.writer.(*kafka.Writer); !ok {
		t.Fatalf("expected writer to be *kafka.Writer, got %T", p.writer)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("closing an unused writer should not error: %v", err)
	}
}

func TestNewProducerWithCustomWriterConfig(t *testing.T) {
	p := NewProducer(ProducerOptions{
		WriterConfig: &kafka.WriterConfig{
			Brokers: []string{"localhost:9092"},
			Topic:   "topic-b",
		},
	})
	if p == nil || p.writer == nil {
		t.Fatal("expected producer with writer")
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("close failed: %v", err)
	}
}

func TestProducerProduce(t *testing.T) {
	ctx := context.Background()

	t.Run("forwards messages to writer", func(t *testing.T) {
		writer := &fakeWriter{}
		p := &Producer{options: ProducerOptions{}, writer: writer}
		msgs := []kafka.Message{
			{Topic: "t", Value: []byte("one")},
			{Topic: "t", Value: []byte("two")},
		}
		if err := p.Produce(ctx, msgs...); err != nil {
			t.Fatalf("Produce failed: %v", err)
		}
		written := writer.written()
		if len(written) != 2 {
			t.Fatalf("expected 2 written messages, got %d", len(written))
		}
		if string(written[0].Value) != "one" || string(written[1].Value) != "two" {
			t.Fatalf("unexpected written values: %q, %q", written[0].Value, written[1].Value)
		}
	})

	t.Run("propagates writer error", func(t *testing.T) {
		writeErr := errors.New("write failed")
		writer := &fakeWriter{writeErr: writeErr}
		p := &Producer{options: ProducerOptions{}, writer: writer}
		err := p.Produce(ctx, kafka.Message{Value: []byte("x")})
		if !errors.Is(err, writeErr) {
			t.Fatalf("expected write error, got %v", err)
		}
	})

	t.Run("logs messages when LogMessage enabled", func(t *testing.T) {
		handler := newCaptureHandler(-8 /* debug */, "sent kafka message")
		prev := slogSwap(handler)
		defer prev()

		writer := &fakeWriter{}
		p := &Producer{options: ProducerOptions{LogMessage: true}, writer: writer}
		if err := p.Produce(ctx, kafka.Message{Topic: "t", Value: []byte("payload")}); err != nil {
			t.Fatalf("Produce failed: %v", err)
		}
		if _, ok := handler.firstAttr("message"); !ok {
			t.Fatal("expected a 'sent kafka message' log record with a message attribute")
		}
	})
}

func TestProducerClose(t *testing.T) {
	writer := &fakeWriter{}
	p := &Producer{options: ProducerOptions{}, writer: writer}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	// Idempotency: closing twice must not panic; error from writer surfaces.
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	if writer.closeCalls != 2 {
		t.Fatalf("expected writer.Close to be called twice, got %d", writer.closeCalls)
	}
}

func headersToMap(headers []kafka.Header) map[string]string {
	m := map[string]string{}
	for _, h := range headers {
		m[h.Key] = string(h.Value)
	}
	return m
}

func TestNewKafkaMessage(t *testing.T) {
	msg := message.NewMessage("uuid-1", []byte("payload"))
	msg.Metadata.Set("trace-id", "abc")

	t.Run("defaults", func(t *testing.T) {
		kmsg := NewKafkaMessage(msg)
		if string(kmsg.Value) != "payload" {
			t.Fatalf("unexpected value: %q", kmsg.Value)
		}
		if len(kmsg.Key) != 0 {
			t.Fatalf("expected empty key, got %q", kmsg.Key)
		}
		got := headersToMap(kmsg.Headers)
		if got[pubsub.MessageIDKey.String()] != "uuid-1" {
			t.Fatalf("expected message id header, got headers %v", got)
		}
		if got["trace-id"] != "abc" {
			t.Fatalf("expected metadata header, got headers %v", got)
		}
	})

	t.Run("with key", func(t *testing.T) {
		kmsg := NewKafkaMessage(msg, WithMessageKey("k1"))
		if string(kmsg.Key) != "k1" {
			t.Fatalf("unexpected key: %q", kmsg.Key)
		}
	})

	t.Run("with headers map and single header", func(t *testing.T) {
		kmsg := NewKafkaMessage(msg,
			WithMessageHeaders(map[string]string{"h1": "v1"}),
			WithMessageHeader("h2", "v2"),
		)
		got := headersToMap(kmsg.Headers)
		if got["h1"] != "v1" || got["h2"] != "v2" {
			t.Fatalf("unexpected headers: %v", got)
		}
		if got[pubsub.MessageIDKey.String()] != "uuid-1" {
			t.Fatalf("message id header missing: %v", got)
		}
	})

	t.Run("single header option initializes nil map", func(t *testing.T) {
		kmsg := NewKafkaMessage(message.NewMessage("u", []byte("p")), WithMessageHeader("only", "one"))
		got := headersToMap(kmsg.Headers)
		if got["only"] != "one" {
			t.Fatalf("unexpected headers: %v", got)
		}
	})
}

func TestNewKafkaMessageJSON(t *testing.T) {
	t.Run("valid data is marshaled", func(t *testing.T) {
		kmsg, err := NewKafkaMessageJSON(map[string]any{"name": "lynx", "n": 42})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(kmsg.Value) != `{"n":42,"name":"lynx"}` {
			t.Fatalf("unexpected value: %s", kmsg.Value)
		}
		if len(kmsg.Key) != 0 {
			t.Fatalf("expected empty key, got %q", kmsg.Key)
		}
		if len(kmsg.Headers) != 0 {
			t.Fatalf("expected no headers, got %v", kmsg.Headers)
		}
	})

	t.Run("options are applied", func(t *testing.T) {
		kmsg, err := NewKafkaMessageJSON("data",
			WithMessageKey("key-1"),
			WithMessageHeader("h", "v"),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(kmsg.Key) != "key-1" {
			t.Fatalf("unexpected key: %q", kmsg.Key)
		}
		got := headersToMap(kmsg.Headers)
		if got["h"] != "v" {
			t.Fatalf("unexpected headers: %v", got)
		}
	})

	// Regression test for commit 31e1db2: json.Marshal errors must be
	// returned instead of being silently ignored.
	t.Run("unmarshalable values return an error", func(t *testing.T) {
		cases := map[string]any{
			"func":    func() {},
			"channel": make(chan int),
		}
		for name, data := range cases {
			t.Run(name, func(t *testing.T) {
				kmsg, err := NewKafkaMessageJSON(data)
				if err == nil {
					t.Fatal("expected a non-nil error for unmarshalable data")
				}
				if kmsg.Value != nil {
					t.Fatalf("expected empty message on error, got value %q", kmsg.Value)
				}
			})
		}
	})
}

// ensure the producer handler adapts a watermill message to a kafka message,
// including the message key metadata.
func TestProducerHandlerPublishesViaProducer(t *testing.T) {
	writer := &fakeWriter{}
	p := &Producer{options: ProducerOptions{}, writer: writer}
	handler := newProducerHandlerFunc(p)

	msg := message.NewMessage("uuid-9", []byte("hello"))
	msg.Metadata.Set(pubsub.MessageKeyKey.String(), "order-1")

	if err := handler(context.Background(), msg); err != nil {
		t.Fatalf("handler failed: %v", err)
	}
	written := writer.written()
	if len(written) != 1 {
		t.Fatalf("expected 1 written message, got %d", len(written))
	}
	if string(written[0].Key) != "order-1" {
		t.Fatalf("expected kafka key from message key metadata, got %q", written[0].Key)
	}
	if string(written[0].Value) != "hello" {
		t.Fatalf("unexpected value: %q", written[0].Value)
	}
}
