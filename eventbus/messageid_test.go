package eventbus

import (
	"context"
	"testing"
	"time"
)

// TestWithMessageIDRoundTrip：WithMessageID 指定的 ID 同时是 Publish 返回值
// 与消费端 Event.ID——生产/消费两端日志凭同一 ID 对账。
func TestWithMessageIDRoundTrip(t *testing.T) {
	b := NewMemoryBus(Options{})
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Start(ctx) }()
	waitRunning(t, b)

	topic := NewTopic[map[string]string]("pin.msgid")
	got := make(chan *Event[map[string]string], 1)
	if err := topic.Subscribe(context.Background(), func(_ context.Context, e *Event[map[string]string]) error {
		got <- e
		return nil
	}, WithBus(b), WithHandlerName("pin-msgid")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	id, err := topic.Publish(context.Background(), map[string]string{"k": "v"},
		WithBus(b), WithMessageID("caller-set-id"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id != "caller-set-id" {
		t.Fatalf("Publish id = %q, want caller-set-id", id)
	}
	select {
	case e := <-got:
		if e.ID != "caller-set-id" {
			t.Fatalf("consumed Event.ID = %q, want caller-set-id", e.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

// TestPublishGeneratedIDMatchesConsumer：未显式指定时由实现生成非空 UUID，
// 返回值与消费端 Event.ID 仍一致。
func TestPublishGeneratedIDMatchesConsumer(t *testing.T) {
	b := NewMemoryBus(Options{})
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Start(ctx) }()
	waitRunning(t, b)

	topic := NewTopic[map[string]string]("pin.msgid.gen")
	got := make(chan *Event[map[string]string], 1)
	if err := topic.Subscribe(context.Background(), func(_ context.Context, e *Event[map[string]string]) error {
		got <- e
		return nil
	}, WithBus(b), WithHandlerName("pin-msgid-gen")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	id, err := topic.Publish(context.Background(), map[string]string{"k": "v"}, WithBus(b))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id == "" {
		t.Fatal("Publish id = empty, want generated UUID")
	}
	select {
	case e := <-got:
		if e.ID != id {
			t.Fatalf("consumed Event.ID = %q, want %q", e.ID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

// TestMessageIDPrecedence：ID 决策序为 WithMessageID > *RawEvent 透传 ID >
// 实现生成；空串选项视为未指定（保留上文结果）。
func TestMessageIDPrecedence(t *testing.T) {
	ctx := context.Background()

	// 默认生成
	raw, err := BuildRawEvent(ctx, nil, "t", []byte("x"), &PublishOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if raw.ID == "" {
		t.Fatal("generated ID empty")
	}

	// *RawEvent 透传保留调用方 ID
	raw, err = BuildRawEvent(ctx, nil, "t",
		&RawEvent{ID: "raw-id", Headers: map[string]string{}}, &PublishOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if raw.ID != "raw-id" {
		t.Fatalf("passthrough ID = %q, want raw-id", raw.ID)
	}

	// WithMessageID 覆盖透传 ID（与 WithMessageKey 对 Key 的覆盖一致）
	raw, err = BuildRawEvent(ctx, nil, "t",
		&RawEvent{ID: "raw-id", Headers: map[string]string{}}, &PublishOptions{MessageID: "opt-id"}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if raw.ID != "opt-id" {
		t.Fatalf("option ID = %q, want opt-id", raw.ID)
	}

	// 空串选项 = 未指定：透传 ID 不被清空
	raw, err = BuildRawEvent(ctx, nil, "t",
		&RawEvent{ID: "raw-id", Headers: map[string]string{}}, &PublishOptions{MessageID: ""}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if raw.ID != "raw-id" {
		t.Fatalf("empty option ID = %q, want raw-id (unset)", raw.ID)
	}
}
