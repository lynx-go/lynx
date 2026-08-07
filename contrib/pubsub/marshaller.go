package pubsub

import (
	"context"
	"encoding/json"
	"fmt"
)

// Marshaler 负责业务对象与消息 Payload 之间的序列化。
// Broker 发布业务对象（Publish 的 payload 非 *Message 时）与类型化订阅
// （Subscribe[T]）都经由它处理。实现需对并发安全（slog.Logger 语义）。
type Marshaler interface {
	// Marshal 将业务对象序列化为 Payload 字节。
	Marshal(any) ([]byte, error)
	// Unmarshal 将 Payload 字节反序列化到 out（指向业务对象的指针）。
	Unmarshal([]byte, any) error
}

// JSONMarshaler 是基于 encoding/json 的默认实现。
type JSONMarshaler struct{}

// Marshal 使用 json.Marshal 序列化。
func (JSONMarshaler) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Unmarshal 使用 json.Unmarshal 反序列化。
func (JSONMarshaler) Unmarshal(data []byte, out any) error {
	return json.Unmarshal(data, out)
}

// TypedMessage 是类型化消息信封：元数据（ID/Key/Headers）与原始 Message
// 一致，Payload 为业务对象。类型化 handler（NewTypedHandler[T]）与直接订阅
// （Subscribe[T]）都以它为处理入参。
type TypedMessage[T any] struct {
	ID      string
	Key     string
	Headers map[string]string
	Payload T
}

// MessageDecoder 是可解码信封：声明自身如何从原始消息填充元数据与
// 业务 Payload。框架处理消息时按 topic 的 Marshaler 调用 Decode，
// handler 无需感知序列化细节。
type MessageDecoder interface {
	Decode(m Marshaler, msg *Message) error
}

// Decode 从原始消息填充信封：拷贝元数据（ID/Key/Headers），Payload 经
// Marshaler 反序列化。T 为 []byte 时表示原始字节语义：直接透传，不经过
// Marshaler。
func (tm *TypedMessage[T]) Decode(m Marshaler, msg *Message) error {
	tm.ID = msg.ID
	tm.Key = msg.Key
	tm.Headers = msg.Headers
	if raw, ok := any(&tm.Payload).(*[]byte); ok {
		*raw = msg.Payload
		return nil
	}
	return m.Unmarshal(msg.Payload, &tm.Payload)
}

// Subscribe 注册类型化订阅：消息 Payload 经 Broker 的 Marshaler 自动
// 反序列化到 T 后调用 h。反序列化失败按处理失败处理（进入重试管线，
// 重试耗尽后不确认，依赖 at-least-once 语义的 Transport 会重投）。
func Subscribe[T any](b Broker, ctx context.Context, topic, handlerName string, h func(ctx context.Context, event *TypedMessage[T]) error, opts ...SubscribeOption) error {
	m := b.MarshalerFor(topic)
	return b.Subscribe(ctx, topic, handlerName, func(ctx context.Context, event *Message) error {
		ev := &TypedMessage[T]{}
		if err := ev.Decode(m, event); err != nil {
			return fmt.Errorf("pubsub: unmarshal payload: %w", err)
		}
		return h(ctx, ev)
	}, opts...)
}
