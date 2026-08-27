package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
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
	go func() { _ = bus.Start(ctx) }()
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

// strictPrefixMarshaler 解码时强制要求前缀：与宽松的 prefixMarshaler 不同，
// 它能暴露"订阅侧实际生效的是哪一个 Marshaler"——优先级若被改错，
// 解码必然失败而不是静默成功。
type strictPrefixMarshaler struct{}

func (strictPrefixMarshaler) Marshal(v any) ([]byte, error) {
	bs, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte("prefix:"), bs...), nil
}

func (strictPrefixMarshaler) Unmarshal(data []byte, out any) error {
	if len(data) < 7 || string(data[:7]) != "prefix:" {
		return fmt.Errorf("strict unmarshal: missing prefix, got %q", string(data))
	}
	return json.Unmarshal(data[7:], out)
}

// TestSubscribeTypedMarshalerResolvedAtSubscribe 是 CORE-03 的回归：
// 解码器提升到订阅时一次解析后，"用户 opts 覆盖 Topic 默认 Marshaler"
// 的优先级语义必须原样保留（两个方向都验证）。
func TestSubscribeTypedMarshalerResolvedAtSubscribe(t *testing.T) {
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	// 方向一：Topic 默认 strictPrefix，用户 opts 显式 JSON——
	// wire 上是纯 JSON（发布侧同优先级），解码必须用用户的 JSON；
	// 若误用 Topic 的 strictPrefix 会因缺前缀而解码失败。
	overridden := NewTopic[map[string]string]("resolve.override", WithTopicMarshaler(strictPrefixMarshaler{}))
	gotOverride := make(chan string, 1)
	if err := SubscribeTyped(context.Background(), bus, overridden, func(ctx context.Context, e *Event[map[string]string]) error {
		gotOverride <- e.Payload["k"]
		return nil
	}, WithHandlerName("h-res-override"), WithSubscribeMarshaler(JSONMarshaler{})); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := overridden.Publish(context.Background(), map[string]string{"k": "user-wins"},
		WithBus(bus), WithPublishMarshaler(JSONMarshaler{})); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case v := <-gotOverride:
		if v != "user-wins" {
			t.Fatalf("override decode got %q, want user-wins", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("user SubscribeOption marshaler did not win — decode failed")
	}

	// 方向二：Topic 默认 strictPrefix，用户未指定——
	// wire 上是带前缀数据，解码必须用 Topic 默认。
	defaulted := NewTopic[map[string]string]("resolve.default", WithTopicMarshaler(strictPrefixMarshaler{}))
	gotDefault := make(chan string, 1)
	if err := SubscribeTyped(context.Background(), bus, defaulted, func(ctx context.Context, e *Event[map[string]string]) error {
		gotDefault <- e.Payload["k"]
		return nil
	}, WithHandlerName("h-res-default")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := defaulted.Publish(context.Background(), map[string]string{"k": "topic-wins"}, WithBus(bus)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case v := <-gotDefault:
		if v != "topic-wins" {
			t.Fatalf("default decode got %q, want topic-wins", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("topic default marshaler not used for decode")
	}
}
