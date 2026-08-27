package eventbus

import "encoding/json"

// Marshaler 负责业务对象与 Payload 的序列化。
type Marshaler interface {
	Marshal(any) ([]byte, error)
	Unmarshal([]byte, any) error
}

// JSONMarshaler 是默认实现。
type JSONMarshaler struct{}

// Marshal 将业务对象序列化为 Payload，直接委托 json.Marshal。
func (JSONMarshaler) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Unmarshal 将 Payload 反序列化到 out，直接委托 json.Unmarshal。
func (JSONMarshaler) Unmarshal(b []byte, out any) error { return json.Unmarshal(b, out) }

// ResolvePublishMarshaler 按对称优先级解析发布侧 Marshaler（高→低）：
// 1. 本次 PublishOption  2. Topic 携带  3. TopicMarshalers  4. 全局  5. JSON。
func ResolvePublishMarshaler(b Bus, topic string, topicMarshaler, optionMarshaler Marshaler) Marshaler {
	if optionMarshaler != nil {
		return optionMarshaler
	}
	if topicMarshaler != nil {
		return topicMarshaler
	}
	if b != nil {
		return b.MarshalerFor(topic)
	}
	return JSONMarshaler{}
}

// ResolveSubscribeMarshaler 按对称优先级解析订阅侧 Marshaler（高→低）：
// 1. 本次 SubscribeOption  2. Topic 携带  3. TopicMarshalers  4. 全局  5. JSON。
func ResolveSubscribeMarshaler(b Bus, topic string, topicMarshaler, optionMarshaler Marshaler) Marshaler {
	return ResolvePublishMarshaler(b, topic, topicMarshaler, optionMarshaler)
}

// TypedDecoding helpers — 内存 Bus 与 Watermill Bus 共用。

// DecodeTyped 将 RawEvent 解码为类型化 Event。
func DecodeTyped[T any](m Marshaler, raw *RawEvent) (*Event[T], error) {
	if m == nil {
		m = JSONMarshaler{}
	}
	ev := &Event[T]{
		ID:      raw.ID,
		Topic:   raw.Topic,
		Key:     raw.Key,
		Headers: raw.Headers,
		Time:    raw.Time,
	}
	if raw.Payload == nil {
		return ev, nil
	}
	// []byte 透传：T 为 []byte 时不经 Marshaler
	if p, ok := any(&ev.Payload).(*[]byte); ok {
		*p = raw.Payload
		return ev, nil
	}
	if err := m.Unmarshal(raw.Payload, &ev.Payload); err != nil {
		return nil, err
	}
	return ev, nil
}
