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

func TestTopicPublishUsesTopicMarshaler(t *testing.T) {
	bus := NewMemoryBus(Options{})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

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
	if err := topic.Publish(context.Background(), payload, WithBus(bus)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case e := <-received:
		// 验证 Payload 是经 prefixMarshaler 序列化的（带前缀）
		if len(e.Payload) < 7 || string(e.Payload[:7]) != "prefix:" {
			t.Fatalf("payload not using topic marshaler, got %q", string(e.Payload))
		}
		// 验证 Topic.Subscribe 也能用同一 Marshaler 正确解码
		typedCh := make(chan string, 1)
		topic2 := NewTopic[map[string]string]("test.custom.typed", WithTopicMarshaler(prefixMarshaler{}))
		if err := topic2.Subscribe(context.Background(), func(ctx context.Context, ev *Event[map[string]string]) error {
			typedCh <- ev.Payload["hello"]
			return nil
		}, WithBus(bus)); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := topic2.Publish(context.Background(), payload, WithBus(bus)); err != nil {
			t.Fatalf("Publish2: %v", err)
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

// TestSubscribeMarshalerResolvedAtSubscribe 是 CORE-03 的回归：
// 解码器提升到订阅时一次解析后，Topic 默认与 Bus 级（MarshalerFor）两条
// 解析路径都必须在订阅时固定并正确生效。
func TestSubscribeMarshalerResolvedAtSubscribe(t *testing.T) {
	// 方向一：Topic 默认 strictPrefix，无调用级覆盖——
	// wire 上是带前缀数据，解码必须用 Topic 默认。
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	defaulted := NewTopic[map[string]string]("resolve.default", WithTopicMarshaler(strictPrefixMarshaler{}))
	gotDefault := make(chan string, 1)
	if err := defaulted.Subscribe(context.Background(), func(ctx context.Context, e *Event[map[string]string]) error {
		gotDefault <- e.Payload["k"]
		return nil
	}, WithBus(bus), WithHandlerName("h-res-default")); err != nil {
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

	// 方向二：Topic 未携带，Bus 级 TopicMarshalers 提供 strictPrefix——
	// 解码走 MarshalerFor 路径，同样在订阅时一次解析。
	bus2 := NewMemoryBus(Options{TopicMarshalers: map[string]Marshaler{
		"resolve.bus-level": strictPrefixMarshaler{},
	}})
	_ = bus2.Init(nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = bus2.Start(ctx2) }()
	waitRunning(t, bus2)

	busLevel := NewTopic[map[string]string]("resolve.bus-level")
	gotBusLevel := make(chan string, 1)
	if err := busLevel.Subscribe(context.Background(), func(ctx context.Context, e *Event[map[string]string]) error {
		gotBusLevel <- e.Payload["k"]
		return nil
	}, WithBus(bus2), WithHandlerName("h-res-bus-level")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := busLevel.Publish(context.Background(), map[string]string{"k": "bus-wins"}, WithBus(bus2)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case v := <-gotBusLevel:
		if v != "bus-wins" {
			t.Fatalf("bus-level decode got %q, want bus-wins", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bus-level (MarshalerFor) marshaler not used for decode")
	}
}
