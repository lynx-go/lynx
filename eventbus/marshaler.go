package eventbus

import "encoding/json"

// Marshaler 负责业务对象与 Payload 的序列化。
type Marshaler interface {
	Marshal(any) ([]byte, error)
	Unmarshal([]byte, any) error
}

// JSONMarshaler 是默认实现。
type JSONMarshaler struct{}

func (JSONMarshaler) Marshal(v any) ([]byte, error)   { return json.Marshal(v) }
func (JSONMarshaler) Unmarshal(b []byte, out any) error { return json.Unmarshal(b, out) }

// TypedDecoding helpers — 内存 Bus 与 Watermill Bus 共用。

// DecodeTyped 将 RawEvent 解码为类型化 Event。
func DecodeTyped[T any](m Marshaler, raw *RawEvent) (*Event[T], error) {
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
