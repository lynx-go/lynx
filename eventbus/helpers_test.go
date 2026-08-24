package eventbus

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// prefixMarshaler 是一个可区分的自定义序列化器：JSON 前加 "prefix:" 前缀
type prefixMarshaler struct{}

func (prefixMarshaler) Marshal(v any) ([]byte, error) {
	bs, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte("prefix:"), bs...), nil
}
func (prefixMarshaler) Unmarshal(data []byte, out any) error {
	// 去掉 prefix:
	if len(data) > 7 && string(data[:7]) == "prefix:" {
		data = data[7:]
	}
	return json.Unmarshal(data, out)
}

func TestPublishTypedUsesTopicMarshaler(t *testing.T) {
	bus := NewMemoryBus(Options{})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bus.Start(ctx)
	// 等待 bus running
	for i := 0; i < 20; i++ {
		if bus.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Topic 带自定义 Marshaler
	topic := NewTopic[map[string]string]("test.custom", WithTopicMarshaler(prefixMarshaler{}))

	received := make(chan *RawEvent, 1)
	if err := bus.Subscribe(context.Background(), topic.Name(), func(ctx context.Context, e *RawEvent) error {
		received <- e
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	payload := map[string]string{"hello": "world"}
	if err := PublishTyped(context.Background(), bus, topic, payload); err != nil {
		t.Fatalf("PublishTyped: %v", err)
	}

	select {
	case e := <-received:
		// 验证 Payload 是经 prefixMarshaler 序列化的（带前缀）
		if len(e.Payload) < 7 || string(e.Payload[:7]) != "prefix:" {
			t.Fatalf("payload not using topic marshaler, got %q", string(e.Payload))
		}
		// 验证 SubscribeTyped 也能用同一 Marshaler 正确解码
		typedCh := make(chan string, 1)
		topic2 := NewTopic[map[string]string]("test.custom.typed", WithTopicMarshaler(prefixMarshaler{}))
		if err := SubscribeTyped(context.Background(), bus, topic2, func(ctx context.Context, ev *Event[map[string]string]) error {
			typedCh <- ev.Payload["hello"]
			return nil
		}); err != nil {
			t.Fatalf("SubscribeTyped: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := PublishTyped(context.Background(), bus, topic2, payload); err != nil {
			t.Fatalf("PublishTyped2: %v", err)
		}
		select {
		case got := <-typedCh:
			if got != "world" {
				t.Fatalf("typed payload = %q, want world", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not receive typed event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive raw event")
	}
}
