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

// TypedMessage 是类型化消息：Payload 为业务对象，而非原始字节。
// 由 Subscribe[T] 反序列化生成，字段与 Message 对应。
type TypedMessage[T any] struct {
	ID      string
	Key     string
	Headers map[string]string
	Payload T
}

// Subscribe 注册类型化订阅：消息 Payload 经 Broker 的 Marshaler 自动
// 反序列化到 T 后调用 h。反序列化失败按处理失败处理（进入重试管线，
// 重试耗尽后不确认，依赖 at-least-once 语义的 Transport 会重投）。
func Subscribe[T any](b Broker, ctx context.Context, topic, handlerName string, h func(ctx context.Context, event *TypedMessage[T]) error, opts ...SubscribeOption) error {
	m := b.MarshalerFor(topic)
	return b.Subscribe(ctx, topic, handlerName, func(ctx context.Context, event *Message) error {
		var payload T
		if err := m.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("pubsub: unmarshal payload: %w", err)
		}
		return h(ctx, &TypedMessage[T]{
			ID:      event.ID,
			Key:     event.Key,
			Headers: event.Headers,
			Payload: payload,
		})
	}, opts...)
}
