package kafka

import (
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/segmentio/kafka-go"
)

func TestToProducerName(t *testing.T) {
	tests := []struct {
		name      string
		eventName string
		expected  string
	}{
		{
			name:      "simple event",
			eventName: "user.created",
			expected:  "user.created:kafka-producer",
		},
		{
			name:      "event with dots",
			eventName: "order.placed",
			expected:  "order.placed:kafka-producer",
		},
		{
			name:      "event with special characters",
			eventName: "user-updated",
			expected:  "user-updated:kafka-producer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToProducerName(tt.eventName)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestToConsumerName(t *testing.T) {
	tests := []struct {
		name      string
		eventName string
		expected  string
	}{
		{
			name:      "simple event",
			eventName: "user.created",
			expected:  "user.created:kafka-consumer",
		},
		{
			name:      "event with dots",
			eventName: "order.placed",
			expected:  "order.placed:kafka-consumer",
		},
		{
			name:      "event with special characters",
			eventName: "user-updated",
			expected:  "user-updated:kafka-consumer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToConsumerName(tt.eventName)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestGetHeader(t *testing.T) {
	t.Run("get existing header", func(t *testing.T) {
		headers := []kafka.Header{
			{Key: "key1", Value: []byte("value1")},
			{Key: "key2", Value: []byte("value2")},
		}

		value := getHeader(headers, "key1")
		if value != "value1" {
			t.Errorf("expected value1, got %s", value)
		}
	})

	t.Run("get non-existing header", func(t *testing.T) {
		headers := []kafka.Header{
			{Key: "key1", Value: []byte("value1")},
		}

		value := getHeader(headers, "non-existing")
		if value != "" {
			t.Errorf("expected empty string, got %s", value)
		}
	})

	t.Run("get header from empty list", func(t *testing.T) {
		headers := []kafka.Header{}

		value := getHeader(headers, "any-key")
		if value != "" {
			t.Errorf("expected empty string, got %s", value)
		}
	})
}

func TestNewMessage(t *testing.T) {
	t.Run("create message from kafka message", func(t *testing.T) {
		kmsg := kafka.Message{
			Value: []byte("test payload"),
			Headers: []kafka.Header{
				{Key: "header1", Value: []byte("value1")},
				{Key: "header2", Value: []byte("value2")},
			},
		}

		msg := NewMessage(kmsg)

		if msg == nil {
			t.Fatal("expected non-nil message")
		}

		if len(msg.Payload) != len(kmsg.Value) {
			t.Errorf("expected payload length %d, got %d", len(kmsg.Value), len(msg.Payload))
		}

		if msg.Metadata.Get("header1") != "value1" {
			t.Errorf("expected header1 = value1, got %s", msg.Metadata.Get("header1"))
		}
	})

	t.Run("create message with message ID", func(t *testing.T) {
		kmsg := kafka.Message{
			Value: []byte("test payload"),
			Headers: []kafka.Header{
				{Key: "x-message-id", Value: []byte("test-id-123")},
			},
		}

		msg := NewMessage(kmsg)

		if msg.UUID != "test-id-123" {
			t.Errorf("expected UUID = test-id-123, got %s", msg.UUID)
		}
	})
}

func TestGetMessageID(t *testing.T) {
	t.Run("get message ID from header", func(t *testing.T) {
		kmsg := &kafka.Message{
			Headers: []kafka.Header{
				{Key: "x-message-id", Value: []byte("msg-id-456")},
			},
		}

		msgID := GetMessageID(kmsg)

		if msgID != "msg-id-456" {
			t.Errorf("expected msg-id-456, got %s", msgID)
		}
	})

	t.Run("get message ID from non-existing header", func(t *testing.T) {
		kmsg := &kafka.Message{
			Headers: []kafka.Header{
				{Key: "other-header", Value: []byte("value")},
			},
		}

		msgID := GetMessageID(kmsg)

		if msgID != "" {
			t.Errorf("expected empty string, got %s", msgID)
		}
	})

	t.Run("get message ID from empty headers", func(t *testing.T) {
		kmsg := kafka.Message{
			Headers: []kafka.Header{},
		}

		msgID := GetMessageID(&kmsg)

		if msgID != "" {
			t.Errorf("expected empty string, got %s", msgID)
		}
	})
}

func TestNewKafkaMessage(t *testing.T) {
	t.Run("create kafka message from watermill message", func(t *testing.T) {
		msg := message.NewMessage("test-uuid", []byte("test payload"))
		msg.Metadata.Set("meta1", "value1")
		msg.Metadata.Set("meta2", "value2")

		kmsg := NewKafkaMessage(msg)

		if len(kmsg.Value) != len(msg.Payload) {
			t.Errorf("expected payload length %d, got %d", len(msg.Payload), len(kmsg.Value))
		}

		msgIDHeaderFound := false
		for _, header := range kmsg.Headers {
			if header.Key == "x-message-id" {
				if string(header.Value) != msg.UUID {
					t.Errorf("expected message ID header = %s, got %s", msg.UUID, string(header.Value))
				}
				msgIDHeaderFound = true
			}
		}

		if !msgIDHeaderFound {
			t.Error("expected x-message-id header to be present")
		}
	})

	t.Run("create kafka message with message key", func(t *testing.T) {
		msg := message.NewMessage("test-uuid", []byte("test payload"))
		kmsg := NewKafkaMessage(msg, WithMessageKey("test-key"))

		if len(kmsg.Key) == 0 {
			t.Error("expected non-empty key")
		}

		if string(kmsg.Key) != "test-key" {
			t.Errorf("expected key = test-key, got %s", string(kmsg.Key))
		}
	})

	t.Run("create kafka message with custom headers", func(t *testing.T) {
		msg := message.NewMessage("test-uuid", []byte("test payload"))
		customHeaders := map[string]string{"custom1": "value1", "custom2": "value2"}
		kmsg := NewKafkaMessage(msg, WithMessageHeaders(customHeaders))

		custom1Found := false
		custom2Found := false

		for _, header := range kmsg.Headers {
			if header.Key == "custom1" {
				if string(header.Value) != "value1" {
					t.Errorf("expected custom1 = value1, got %s", string(header.Value))
				}
				custom1Found = true
			}
			if header.Key == "custom2" {
				if string(header.Value) != "value2" {
					t.Errorf("expected custom2 = value2, got %s", string(header.Value))
				}
				custom2Found = true
			}
		}

		if !custom1Found {
			t.Error("expected custom1 header to be present")
		}

		if !custom2Found {
			t.Error("expected custom2 header to be present")
		}
	})

	t.Run("create kafka message with single header", func(t *testing.T) {
		msg := message.NewMessage("test-uuid", []byte("test payload"))
		kmsg := NewKafkaMessage(msg, WithMessageHeader("single-header", "single-value"))

		found := false
		for _, header := range kmsg.Headers {
			if header.Key == "single-header" {
				if string(header.Value) != "single-value" {
					t.Errorf("expected single-value, got %s", string(header.Value))
				}
				found = true
				break
			}
		}

		if !found {
			t.Error("expected single-header to be present")
		}
	})
}

func TestNewKafkaMessageJSON(t *testing.T) {
	t.Run("create kafka message from JSON data", func(t *testing.T) {
		data := map[string]string{"key": "value"}
		kmsg := NewKafkaMessageJSON(data)

		if len(kmsg.Value) == 0 {
			t.Error("expected non-empty payload")
		}

		if string(kmsg.Value) != `{"key":"value"}` {
			t.Errorf("expected JSON payload, got %s", string(kmsg.Value))
		}
	})

	t.Run("create kafka message with key", func(t *testing.T) {
		data := map[string]int{"num": 42}
		kmsg := NewKafkaMessageJSON(data, WithMessageKey("json-key"))

		if string(kmsg.Key) != "json-key" {
			t.Errorf("expected key = json-key, got %s", string(kmsg.Key))
		}
	})

	t.Run("create kafka message with headers", func(t *testing.T) {
		data := "simple string"
		headers := map[string]string{"h1": "v1", "h2": "v2"}
		kmsg := NewKafkaMessageJSON(data, WithMessageHeaders(headers))

		h1Found := false
		h2Found := false

		for _, header := range kmsg.Headers {
			if header.Key == "h1" && string(header.Value) == "v1" {
				h1Found = true
			}
			if header.Key == "h2" && string(header.Value) == "v2" {
				h2Found = true
			}
		}

		if !h1Found || !h2Found {
			t.Error("expected custom headers to be present")
		}
	})
}
