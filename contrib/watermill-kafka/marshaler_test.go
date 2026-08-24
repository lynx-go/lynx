package kafka

import (
	"testing"

	"github.com/IBM/sarama"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/eventbus"
)

func TestWireMarshalerSetsKafkaRecordKey(t *testing.T) {
	msg := message.NewMessage("id-1", []byte("payload"))
	msg.Metadata.Set(eventbus.MetaMessageKey, "order-42")
	msg.Metadata.Set(eventbus.MetaLogicalTopic, "orders")

	kafkaMsg, err := wireMarshaler{}.Marshal("physical_orders", msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	key, ok := kafkaMsg.Key.(sarama.ByteEncoder)
	if !ok {
		t.Fatalf("Key type = %T, want sarama.ByteEncoder", kafkaMsg.Key)
	}
	if string(key) != "order-42" {
		t.Fatalf("Kafka record Key = %q, want order-42", string(key))
	}
	// metadata 仍进 headers（含 x-message-key），与 DefaultMarshaler 一致。
	found := false
	for _, h := range kafkaMsg.Headers {
		if string(h.Key) == eventbus.MetaMessageKey && string(h.Value) == "order-42" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected x-message-key header")
	}
}

func TestWireMarshalerEmptyKeyOmitsRecordKey(t *testing.T) {
	msg := message.NewMessage("id-1", []byte("payload"))
	kafkaMsg, err := wireMarshaler{}.Marshal("t", msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if kafkaMsg.Key != nil {
		t.Fatalf("Key = %v, want nil when x-message-key absent", kafkaMsg.Key)
	}
}

func TestWireMarshalerUnmarshalRestoresKeyFromRecord(t *testing.T) {
	// 外部生产者可能只写 Kafka Key、无 x-message-key header。
	kafkaMsg := &sarama.ConsumerMessage{
		Key:   []byte("partition-key"),
		Value: []byte("body"),
		Headers: []*sarama.RecordHeader{{
			Key:   []byte(watermillkafka.UUIDHeaderKey),
			Value: []byte("uuid-1"),
		}},
	}
	msg, err := wireMarshaler{}.Unmarshal(kafkaMsg)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := msg.Metadata.Get(eventbus.MetaMessageKey); got != "partition-key" {
		t.Fatalf("x-message-key = %q, want partition-key", got)
	}
	raw := fromWatermill(msg)
	if raw.Key != "partition-key" {
		t.Fatalf("RawEvent.Key = %q, want partition-key", raw.Key)
	}
}

func TestWireMarshalerUnmarshalPrefersHeaderOverRecordKey(t *testing.T) {
	kafkaMsg := &sarama.ConsumerMessage{
		Key:   []byte("record-key"),
		Value: []byte("body"),
		Headers: []*sarama.RecordHeader{
			{Key: []byte(watermillkafka.UUIDHeaderKey), Value: []byte("uuid-1")},
			{Key: []byte(eventbus.MetaMessageKey), Value: []byte("header-key")},
		},
	}
	msg, err := wireMarshaler{}.Unmarshal(kafkaMsg)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := msg.Metadata.Get(eventbus.MetaMessageKey); got != "header-key" {
		t.Fatalf("x-message-key = %q, want header-key", got)
	}
}

func TestPublishRoundTripWireMetadata(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Producer: &ProducerOptions{},
			},
		},
	}, pub)
	in := &eventbus.RawEvent{
		ID:      "evt-1",
		Topic:   "orders",
		Key:     "user-7",
		Payload: []byte(`{"n":1}`),
		Headers: map[string]string{"request_id": "r1"},
	}
	if err := tr.Publish(t.Context(), "orders", in); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgs := pub.publishedMsgs()
	if len(msgs) != 1 {
		t.Fatalf("published %d msgs, want 1", len(msgs))
	}
	got := fromWatermill(msgs[0])
	if got.ID != "evt-1" || got.Key != "user-7" || got.Topic != "orders" {
		t.Fatalf("round-trip RawEvent = %+v", got)
	}
	if string(got.Payload) != `{"n":1}` || got.Headers["request_id"] != "r1" {
		t.Fatalf("payload/headers: %+v", got)
	}
}
