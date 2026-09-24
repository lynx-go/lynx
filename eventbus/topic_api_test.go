package eventbus

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMarshalerPriorityOptionOverTopic(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	topic := NewTopic[map[string]string]("prio.opt", WithTopicMarshaler(prefixMarshaler{}))
	received := make(chan []byte, 1)
	_ = bus.Subscribe(context.Background(), topic.Name(), func(ctx context.Context, e *RawEvent) error {
		received <- append([]byte(nil), e.Payload...)
		return nil
	})
	time.Sleep(20 * time.Millisecond)

	// Option marshaler (JSON) must win over Topic prefix marshaler
	if err := topic.Publish(context.Background(), map[string]string{"a": "b"},
		WithBus(bus), WithPublishMarshaler(JSONMarshaler{})); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case p := <-received:
		if len(p) >= 7 && string(p[:7]) == "prefix:" {
			t.Fatalf("option marshaler should win, got prefix payload %q", p)
		}
		var m map[string]string
		if err := json.Unmarshal(p, &m); err != nil || m["a"] != "b" {
			t.Fatalf("payload = %q, err=%v", p, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

// TestMarshalerPriorityTopicMarshalersOverTopicsConfig 验证 MarshalerFor 同级两个面的先后：
// TopicMarshalers[t] 赢过 Topics[t].Marshaler（均为编程式设置面）。
func TestMarshalerPriorityTopicMarshalersOverTopicsConfig(t *testing.T) {
	bus := NewMemoryBus(Options{
		TopicMarshalers: map[string]Marshaler{"prio.cfg": JSONMarshaler{}},
		Topics:          map[string]TopicConfig{"prio.cfg": {Marshaler: prefixMarshaler{}}},
	})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	received := make(chan []byte, 1)
	_ = bus.Subscribe(context.Background(), "prio.cfg", func(ctx context.Context, e *RawEvent) error {
		received <- append([]byte(nil), e.Payload...)
		return nil
	})
	time.Sleep(20 * time.Millisecond)

	if err := bus.Publish(context.Background(), "prio.cfg", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case p := <-received:
		// TopicMarshalers 的 JSON 应赢：payload 无 prefix: 前缀
		if len(p) >= 7 && string(p[:7]) == "prefix:" {
			t.Fatalf("TopicMarshalers should win over Topics[t].Marshaler, got prefix payload %q", p)
		}
		var m map[string]string
		if err := json.Unmarshal(p, &m); err != nil || m["a"] != "b" {
			t.Fatalf("payload = %q, err=%v", p, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

// TestMarshalerTopicsConfigFallback 验证 Topics[t].Marshaler 作为 Bus 级回填生效：
// 高于全局默认（JSON）。
func TestMarshalerTopicsConfigFallback(t *testing.T) {
	bus := NewMemoryBus(Options{
		Topics: map[string]TopicConfig{"prio.fallback": {Marshaler: prefixMarshaler{}}},
	})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	received := make(chan []byte, 1)
	_ = bus.Subscribe(context.Background(), "prio.fallback", func(ctx context.Context, e *RawEvent) error {
		received <- append([]byte(nil), e.Payload...)
		return nil
	})
	time.Sleep(20 * time.Millisecond)

	if err := bus.Publish(context.Background(), "prio.fallback", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case p := <-received:
		if len(p) < 7 || string(p[:7]) != "prefix:" {
			t.Fatalf("Topics[t].Marshaler should be used, got %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestPublishRawEventUsesParamTopic(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	got := make(chan string, 1)
	_ = bus.Subscribe(context.Background(), "logical.param", func(ctx context.Context, e *RawEvent) error {
		got <- e.Topic
		return nil
	})
	time.Sleep(20 * time.Millisecond)

	raw := &RawEvent{ID: "keep", Topic: "spoof.topic", Key: "k", Payload: []byte("x"), Time: time.Unix(1, 0)}
	if err := bus.Publish(context.Background(), "logical.param", raw); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case topic := <-got:
		if topic != "logical.param" {
			t.Fatalf("topic = %q, want logical.param (param wins over RawEvent.Topic)", topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

// TestTopicPublishForwardsRawEnvelope 转发入口收敛后：Topic.Publish 收到
// *RawEvent 时整份信封透传（保留 ID/Key/Headers/Time），逻辑名以 Topic 为准。
func TestTopicPublishForwardsRawEnvelope(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	ts := time.Unix(100, 0).UTC()
	topic := NewTopic[*RawEvent]("fwd.topic")
	got := make(chan *RawEvent, 1)
	_ = bus.Subscribe(context.Background(), topic.Name(), func(ctx context.Context, e *RawEvent) error {
		got <- e
		return nil
	})
	time.Sleep(20 * time.Millisecond)

	raw := &RawEvent{
		ID:      "orig-id",
		Topic:   "ignored",
		Key:     "orig-key",
		Headers: map[string]string{"h": "v"},
		Payload: []byte("body"),
		Time:    ts,
	}
	if err := topic.Publish(context.Background(), raw, WithBus(bus)); err != nil {
		t.Fatalf("Publish(*RawEvent): %v", err)
	}
	select {
	case e := <-got:
		if e.ID != "orig-id" {
			t.Errorf("ID = %q, want orig-id", e.ID)
		}
		if e.Key != "orig-key" {
			t.Errorf("Key = %q, want orig-key", e.Key)
		}
		if e.Headers["h"] != "v" {
			t.Errorf("Headers = %v", e.Headers)
		}
		if !e.Time.Equal(ts) {
			t.Errorf("Time = %v, want %v", e.Time, ts)
		}
		if e.Topic != topic.Name() {
			t.Errorf("Topic = %q, want %q", e.Topic, topic.Name())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

// TestTopicPublishRawPayloadsBypassMarshaler：原始载荷（*RawEvent / []byte）
// 按透传处理，Topic 级 marshaler 被忽略——语义钉死而非注释承诺。
func TestTopicPublishRawPayloadsBypassMarshaler(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	topicBytes := NewTopic[[]byte]("raw.bypass", WithTopicMarshaler(prefixMarshaler{}))
	topicRaw := NewTopic[*RawEvent]("raw.bypass", WithTopicMarshaler(prefixMarshaler{}))
	got := make(chan []byte, 2)
	_ = bus.Subscribe(context.Background(), topicBytes.Name(), func(ctx context.Context, e *RawEvent) error {
		got <- e.Payload
		return nil
	})
	time.Sleep(20 * time.Millisecond)

	// []byte 载荷：跳过序列化直发。
	if err := topicBytes.Publish(context.Background(), []byte("bytes-body"), WithBus(bus)); err != nil {
		t.Fatalf("Publish([]byte): %v", err)
	}
	select {
	case p := <-got:
		if string(p) != "bytes-body" {
			t.Fatalf("[]byte payload = %q, want bytes-body (marshaler must be bypassed)", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}

	// *RawEvent 载荷：整份信封透传，Payload 原样。
	raw := &RawEvent{ID: "r1", Payload: []byte("raw-body"), Time: time.Now()}
	if err := topicRaw.Publish(context.Background(), raw, WithBus(bus)); err != nil {
		t.Fatalf("Publish(*RawEvent): %v", err)
	}
	select {
	case p := <-got:
		if string(p) != "raw-body" {
			t.Fatalf("*RawEvent payload = %q, want raw-body (marshaler must be bypassed)", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func waitRunning(t *testing.T, bus Bus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.CheckHealth() == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("bus not running")
}
