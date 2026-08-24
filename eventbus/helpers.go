package eventbus

import (
	"context"
	"fmt"
)

// PublishTyped 发布类型化负载，自动经 Topic/Marshaler 序列化。
// 通过 WithPublishMarshaler 将 Topic 携带的 Marshaler 注入 Bus.Publish，复用 Bus 内部
// 的序列化、头部合并、属性传播等统一逻辑，与 SubscribeTyped 的解码侧保持一致。
func PublishTyped[T any](ctx context.Context, b Bus, topic Topic[T], payload T, opts ...PublishOption) error {
	if m := topic.Options().Marshaler; m != nil {
		opts = append(append([]PublishOption(nil), opts...), WithPublishMarshaler(m))
	}
	return b.Publish(ctx, topic.Name(), payload, opts...)
}

// SubscribeTyped 订阅类型化主题，Payload 自动反序列化为 T。
func SubscribeTyped[T any](ctx context.Context, b Bus, topic Topic[T], handlerName string, h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	m := b.MarshalerFor(topic.Name())
	// Topic 级 Marshaler 优先，并通过 WithSubscribeMarshaler 注入 Bus 侧，保持与 PublishTyped 对称
	if topic.Options().Marshaler != nil {
		m = topic.Options().Marshaler
	}
	topts := topic.Options()
	wrappedOpts := make([]SubscribeOption, 0, len(opts)+5)
	if topts.Group != "" {
		wrappedOpts = append(wrappedOpts, WithGroup(topts.Group))
	}
	if topts.Instances != 0 {
		wrappedOpts = append(wrappedOpts, WithInstances(topts.Instances))
	}
	if topts.AutoAck {
		wrappedOpts = append(wrappedOpts, WithAutoAck())
	}
	if topts.ContinueOnError {
		wrappedOpts = append(wrappedOpts, WithContinueOnError())
	}
	if m != nil {
		wrappedOpts = append(wrappedOpts, WithSubscribeMarshaler(m))
	}
	wrappedOpts = append(wrappedOpts, opts...)

	return b.Subscribe(ctx, topic.Name(), handlerName, func(ctx context.Context, raw *RawEvent) error {
		ev, err := DecodeTyped[T](m, raw)
		if err != nil {
			return fmt.Errorf("bus: unmarshal %q: %w", topic.Name(), err)
		}
		return h(ctx, ev)
	}, wrappedOpts...)
}

// PublishRawTyped 以原始事件发布类型化主题（用于转发）。
func PublishRawTyped[T any](ctx context.Context, b Bus, topic Topic[T], raw *RawEvent, opts ...PublishOption) error {
	if raw == nil {
		return fmt.Errorf("bus: nil raw event")
	}
	return b.PublishRaw(ctx, topic.Name(), raw.Payload, append([]PublishOption{WithMessageKey(raw.Key), WithMetadata(raw.Headers)}, opts...)...)
}
