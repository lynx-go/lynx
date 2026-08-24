package eventbus

import (
	"maps"
	"testing"
	"time"
)

func TestWireRoundTripPreservesFields(t *testing.T) {
	ts := time.Date(2026, 8, 24, 12, 0, 0, 123456789, time.UTC)
	in := &RawEvent{
		ID:      "id-1",
		Topic:   "order.created",
		Key:     "order-42",
		Headers: map[string]string{"foo": "bar", "user_id": "u1"},
		Payload: []byte(`{"id":"42"}`),
		Time:    ts,
	}

	meta := EncodeWireMetadata(in)
	out := DecodeWireMetadata(in.ID, in.Payload, meta)

	if out.ID != in.ID {
		t.Errorf("ID = %q, want %q", out.ID, in.ID)
	}
	if out.Topic != in.Topic {
		t.Errorf("Topic = %q, want %q", out.Topic, in.Topic)
	}
	if out.Key != in.Key {
		t.Errorf("Key = %q, want %q", out.Key, in.Key)
	}
	if !out.Time.Equal(in.Time) {
		t.Errorf("Time = %v, want %v", out.Time, in.Time)
	}
	if string(out.Payload) != string(in.Payload) {
		t.Errorf("Payload = %q, want %q", out.Payload, in.Payload)
	}
	if out.Headers["foo"] != "bar" || out.Headers["user_id"] != "u1" {
		t.Errorf("Headers = %v, want foo/bar user_id/u1", out.Headers)
	}
	if _, ok := out.Headers[MetaMessageKey]; ok {
		t.Error("protocol key x-message-key must not remain in Headers")
	}
	if _, ok := out.Headers[MetaEventTime]; ok {
		t.Error("protocol key x-event-time must not remain in Headers")
	}
	if _, ok := out.Headers[MetaLogicalTopic]; ok {
		t.Error("protocol key x-logical-topic must not remain in Headers")
	}
}

func TestWireKeyNotOverwrittenByHeaders(t *testing.T) {
	in := &RawEvent{
		ID:      "id-2",
		Topic:   "t",
		Key:     "real-key",
		Headers: map[string]string{MetaMessageKey: "spoofed", "ok": "1"},
		Payload: []byte("x"),
		Time:    time.Unix(1, 0).UTC(),
	}
	meta := EncodeWireMetadata(in)
	if got := meta[MetaMessageKey]; got != "real-key" {
		t.Fatalf("x-message-key = %q, want real-key (Headers must not overwrite)", got)
	}
	out := DecodeWireMetadata(in.ID, in.Payload, meta)
	if out.Key != "real-key" {
		t.Fatalf("Key = %q, want real-key", out.Key)
	}
	if _, ok := out.Headers[MetaMessageKey]; ok {
		t.Fatal("x-message-key leaked into Headers")
	}
	if out.Headers["ok"] != "1" {
		t.Fatalf("Headers[ok] = %q, want 1", out.Headers["ok"])
	}
}

func TestWireTimeMissingFallsBack(t *testing.T) {
	before := time.Now()
	meta := map[string]string{"foo": "bar"}
	out := DecodeWireMetadata("id", []byte("p"), meta)
	if out.Time.Before(before) {
		t.Fatalf("Time = %v, want >= %v (fallback now)", out.Time, before)
	}
	if out.Headers["foo"] != "bar" {
		t.Fatalf("Headers = %v", out.Headers)
	}
}

func TestWireEncodeSkipsEmptyProtocolFields(t *testing.T) {
	in := &RawEvent{
		ID:      "id",
		Payload: []byte("p"),
		Headers: map[string]string{"a": "b"},
	}
	meta := EncodeWireMetadata(in)
	if _, ok := meta[MetaMessageKey]; ok {
		t.Error("empty Key should not set x-message-key")
	}
	if _, ok := meta[MetaLogicalTopic]; ok {
		t.Error("empty Topic should not set x-logical-topic")
	}
	if _, ok := meta[MetaEventTime]; ok {
		t.Error("zero Time should not set x-event-time")
	}
	want := map[string]string{"a": "b"}
	if !maps.Equal(filterNonProtocol(meta), want) {
		t.Fatalf("meta headers = %v, want %v", meta, want)
	}
}

func filterNonProtocol(meta map[string]string) map[string]string {
	out := maps.Clone(meta)
	delete(out, MetaMessageKey)
	delete(out, MetaEventTime)
	delete(out, MetaLogicalTopic)
	return out
}
