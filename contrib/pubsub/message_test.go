package pubsub

import (
	"context"
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
)

func TestNewMessageDefaults(t *testing.T) {
	m := NewMessage([]byte("payload"))
	if m.ID == "" {
		t.Fatal("expected non-empty random ID")
	}
	if m.Key != "" {
		t.Fatalf("expected empty key, got %q", m.Key)
	}
	if m.Headers == nil {
		t.Fatal("expected non-nil headers map")
	}
	if string(m.Payload) != "payload" {
		t.Fatalf("unexpected payload: %q", m.Payload)
	}
}

func TestMessageOptions(t *testing.T) {
	m := NewMessage(nil,
		WithID("id-1"),
		WithKey("key-1"),
		WithHeader("a", "1"),
		WithHeaders(map[string]string{"b": "2"}),
	)
	if m.ID != "id-1" || m.Key != "key-1" {
		t.Fatalf("unexpected id/key: %q %q", m.ID, m.Key)
	}
	if m.Headers["a"] != "1" || m.Headers["b"] != "2" {
		t.Fatalf("unexpected headers: %+v", m.Headers)
	}
}

func TestToWatermill(t *testing.T) {
	m := NewMessage([]byte("x"), WithID("id-1"), WithKey("k1"), WithHeader("h", "v"))
	wm := toWatermill(m)
	if wm.UUID != "id-1" {
		t.Fatalf("unexpected uuid: %q", wm.UUID)
	}
	if string(wm.Payload) != "x" {
		t.Fatalf("unexpected payload: %q", wm.Payload)
	}
	if got := wm.Metadata.Get(MessageKeyKey.String()); got != "k1" {
		t.Fatalf("unexpected key in metadata: %q", got)
	}
	if got := wm.Metadata.Get("h"); got != "v" {
		t.Fatalf("unexpected header: %q", got)
	}
}

func TestFromWatermill(t *testing.T) {
	wm := message.NewMessage("id-1", []byte("x"))
	wm.Metadata.Set(MessageKeyKey.String(), "k1")
	wm.Metadata.Set("h", "v")
	m := fromWatermill(wm)
	if m.ID != "id-1" || m.Key != "k1" || string(m.Payload) != "x" {
		t.Fatalf("unexpected message: %+v", m)
	}
	if m.Headers["h"] != "v" {
		t.Fatalf("unexpected header: %+v", m.Headers)
	}
	// 协议键 x-message-key 不进入 Headers。
	if _, ok := m.Headers[MessageKeyKey.String()]; ok {
		t.Fatalf("protocol key leaked into Headers: %+v", m.Headers)
	}
}

func TestRoundTrip(t *testing.T) {
	m := NewMessage([]byte("payload"), WithKey("k1"), WithHeader("trace-id", "abc"))
	got := fromWatermill(toWatermill(m))
	if got.ID != m.ID || got.Key != m.Key || string(got.Payload) != string(m.Payload) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, m)
	}
	if got.Headers["trace-id"] != "abc" {
		t.Fatalf("round-trip header mismatch: %+v", got.Headers)
	}
}

func TestMessageContextHelpers(t *testing.T) {
	// 从旧 broker.go 迁移的 helpers 行为不变。
	if got := MessageIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty id, got %q", got)
	}
	ctx := ContextWithMessageID(context.Background(), "msg-1")
	if got := MessageIDFromContext(ctx); got != "msg-1" {
		t.Fatalf("expected msg-1, got %q", got)
	}
	ctx = ContextWithMessageKey(ctx, "key-1")
	if got := MessageKeyFromContext(ctx); got != "key-1" {
		t.Fatalf("expected key-1, got %q", got)
	}
}
