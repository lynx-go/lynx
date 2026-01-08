package pubsub

import (
	"context"
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
)

func TestNewJSONMessage(t *testing.T) {
	tests := []struct {
		name string
		data interface{}
	}{
		{
			name: "simple object",
			data: map[string]string{"key": "value"},
		},
		{
			name: "array",
			data: []int{1, 2, 3},
		},
		{
			name: "string",
			data: "test",
		},
		{
			name: "nil",
			data: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := NewJSONMessage(tt.data)

			if msg == nil {
				t.Fatal("expected non-nil message")
			}

			if msg.UUID == "" {
				t.Error("expected non-empty UUID")
			}

			if len(msg.Payload) == 0 && tt.data != nil {
				t.Error("expected non-empty payload for non-nil data")
			}
		})
	}
}

func TestMessageContext(t *testing.T) {
	t.Run("ContextWithMessageID", func(t *testing.T) {
		ctx := context.Background()
		msgId := "test-msg-id"

		newCtx := ContextWithMessageID(ctx, msgId)

		retrievedId := MessageIDFromContext(newCtx)
		if retrievedId != msgId {
			t.Errorf("expected msgId = %s, got %s", msgId, retrievedId)
		}

		originalId, ok := ctx.Value(MessageIDKey).(string)
		if ok && originalId == msgId {
			t.Error("expected original context to not have message ID")
		}
	})

	t.Run("ContextWithMessageKey", func(t *testing.T) {
		ctx := context.Background()
		msgKey := "test-msg-key"

		newCtx := ContextWithMessageKey(ctx, msgKey)

		retrievedKey := MessageKeyFromContext(newCtx)
		if retrievedKey != msgKey {
			t.Errorf("expected msgKey = %s, got %s", msgKey, retrievedKey)
		}

		originalKey := MessageKeyFromContext(ctx)
		if originalKey == msgKey {
			t.Error("expected original context to not have message key")
		}
	})

	t.Run("multiple context values", func(t *testing.T) {
		ctx := context.Background()
		msgId := "msg-id-123"
		msgKey := "msg-key-456"

		ctx = ContextWithMessageID(ctx, msgId)
		ctx = ContextWithMessageKey(ctx, msgKey)

		retrievedId := MessageIDFromContext(ctx)
		if retrievedId != msgId {
			t.Errorf("expected msgId = %s, got %s", msgId, retrievedId)
		}

		retrievedKey := MessageKeyFromContext(ctx)
		if retrievedKey != msgKey {
			t.Errorf("expected msgKey = %s, got %s", msgKey, retrievedKey)
		}
	})
}

func TestMessageMetadata(t *testing.T) {
	t.Run("SetMessageKey and GetMessageKey", func(t *testing.T) {
		msg := message.NewMessage("test-id", []byte("test payload"))
		key := "message-key-123"

		SetMessageKey(msg, key)
		retrievedKey := GetMessageKey(msg)

		if retrievedKey != key {
			t.Errorf("expected key = %s, got %s", key, retrievedKey)
		}
	})

	t.Run("SetMessageID and GetMessageID", func(t *testing.T) {
		msg := message.NewMessage("test-id", []byte("test payload"))
		id := "message-id-456"

		SetMessageID(msg, id)
		retrievedId := GetMessageID(msg)

		if retrievedId != id {
			t.Errorf("expected id = %s, got %s", id, retrievedId)
		}
	})
}

func TestPublishOptions(t *testing.T) {
	t.Run("WithMessageKey", func(t *testing.T) {
		opts := &PublishOptions{}
		WithMessageKey("test-key")(opts)

		if opts.MessageKey != "test-key" {
			t.Errorf("expected MessageKey = test-key, got %s", opts.MessageKey)
		}
	})

	t.Run("WithMetadata", func(t *testing.T) {
		metadata := map[string]string{"key1": "value1", "key2": "value2"}
		opts := &PublishOptions{}
		WithMetadata(metadata)(opts)

		if opts.Metadata["key1"] != "value1" {
			t.Errorf("expected metadata key1 = value1, got %s", opts.Metadata["key1"])
		}

		if opts.Metadata["key2"] != "value2" {
			t.Errorf("expected metadata key2 = value2, got %s", opts.Metadata["key2"])
		}
	})

	t.Run("WithMetadataField", func(t *testing.T) {
		opts := &PublishOptions{}
		WithMetadataField("key1", "value1")(opts)
		WithMetadataField("key2", "value2")(opts)

		if opts.Metadata["key1"] != "value1" {
			t.Errorf("expected metadata key1 = value1, got %s", opts.Metadata["key1"])
		}

		if opts.Metadata["key2"] != "value2" {
			t.Errorf("expected metadata key2 = value2, got %s", opts.Metadata["key2"])
		}
	})

	t.Run("WithFromBinder", func(t *testing.T) {
		opts := &PublishOptions{}
		WithFromBinder()(opts)

		if !opts.FromBinder {
			t.Error("expected FromBinder to be true")
		}
	})

	t.Run("multiple options", func(t *testing.T) {
		opts := &PublishOptions{}
		WithMessageKey("test-key")(opts)
		WithFromBinder()(opts)
		WithMetadataField("custom", "value")(opts)

		if opts.MessageKey != "test-key" {
			t.Errorf("expected MessageKey = test-key, got %s", opts.MessageKey)
		}

		if !opts.FromBinder {
			t.Error("expected FromBinder to be true")
		}

		if opts.Metadata["custom"] != "value" {
			t.Errorf("expected metadata custom = value, got %s", opts.Metadata["custom"])
		}
	})
}

func TestSubscribeOptions(t *testing.T) {
	t.Run("WithAutoAck", func(t *testing.T) {
		opts := &SubscribeOptions{}
		WithAutoAck()(opts)

		if !opts.AutoAck {
			t.Error("expected AutoAck to be true")
		}
	})

	t.Run("WithContinueOnError", func(t *testing.T) {
		opts := &SubscribeOptions{}
		WithContinueOnError()(opts)

		if !opts.ContinueOnError {
			t.Error("expected ContinueOnError to be true")
		}
	})

	t.Run("multiple options", func(t *testing.T) {
		opts := &SubscribeOptions{}
		WithAutoAck()(opts)
		WithContinueOnError()(opts)

		if !opts.AutoAck {
			t.Error("expected AutoAck to be true")
		}

		if !opts.ContinueOnError {
			t.Error("expected ContinueOnError to be true")
		}
	})
}

func TestMessageKeyKey(t *testing.T) {
	keyStr := MessageKeyKey.String()
	expectedKey := "x-message-key"

	if keyStr != expectedKey {
		t.Errorf("expected MessageKeyKey.String() = %s, got %s", expectedKey, keyStr)
	}
}

func TestMessageIDKey(t *testing.T) {
	keyStr := MessageIDKey.String()
	expectedKey := "x-message-id"

	if keyStr != expectedKey {
		t.Errorf("expected MessageIDKey.String() = %s, got %s", expectedKey, keyStr)
	}
}
