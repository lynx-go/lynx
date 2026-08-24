package eventbus

import (
	"context"
	"fmt"
)

// Publish 发布类型化负载。Bus 解析：WithBus Option → Context → Default。
func (t Topic[T]) Publish(ctx context.Context, payload T, opts ...PublishOption) error {
	po := &PublishOptions{}
	applyPublishOptions(po, opts...)
	b, err := resolveBus(ctx, po.Bus)
	if err != nil {
		return err
	}
	return publishTyped(ctx, b, t, payload, opts...)
}

// Subscribe 订阅类型化主题。Bus 解析同 Publish。
func (t Topic[T]) Subscribe(ctx context.Context, handlerName string, h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	so := &SubscribeOptions{}
	applySubscribeOptions(so, opts...)
	b, err := resolveBus(ctx, so.Bus)
	if err != nil {
		return err
	}
	return subscribeTyped(ctx, b, t, handlerName, h, opts...)
}

// PublishRaw 转发原始事件（保留 ID/Key/Headers/Time/Payload；逻辑名以 Topic 为准）。
func (t Topic[T]) PublishRaw(ctx context.Context, raw *RawEvent, opts ...PublishOption) error {
	po := &PublishOptions{}
	applyPublishOptions(po, opts...)
	b, err := resolveBus(ctx, po.Bus)
	if err != nil {
		return err
	}
	return publishRawTyped(ctx, b, t, raw, opts...)
}

// PublishTyped 发布类型化负载（迁移期薄别名；新代码优先 Topic.Publish）。
func PublishTyped[T any](ctx context.Context, b Bus, topic Topic[T], payload T, opts ...PublishOption) error {
	if b == nil {
		return ErrNoBus
	}
	return publishTyped(ctx, b, topic, payload, opts...)
}

// SubscribeTyped 订阅类型化主题（迁移期薄别名；新代码优先 Topic.Subscribe）。
func SubscribeTyped[T any](ctx context.Context, b Bus, topic Topic[T], handlerName string, h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	if b == nil {
		return ErrNoBus
	}
	return subscribeTyped(ctx, b, topic, handlerName, h, opts...)
}

// PublishRawTyped 以原始事件发布类型化主题（迁移期薄别名；新代码优先 Topic.PublishRaw）。
func PublishRawTyped[T any](ctx context.Context, b Bus, topic Topic[T], raw *RawEvent, opts ...PublishOption) error {
	if b == nil {
		return ErrNoBus
	}
	return publishRawTyped(ctx, b, topic, raw, opts...)
}

func publishTyped[T any](ctx context.Context, b Bus, topic Topic[T], payload T, opts ...PublishOption) error {
	po := &PublishOptions{}
	applyPublishOptions(po, opts...)
	// Topic Marshaler 作为较低优先级默认：先注入，再让调用方 opts 覆盖
	var base []PublishOption
	if m := topic.Options().Marshaler; m != nil {
		base = append(base, WithPublishMarshaler(m))
	}
	base = append(base, opts...)
	return b.Publish(ctx, topic.Name(), payload, base...)
}

func subscribeTyped[T any](ctx context.Context, b Bus, topic Topic[T], handlerName string, h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	so := &SubscribeOptions{}
	applySubscribeOptions(so, opts...)
	topts := topic.Options()
	m := ResolveSubscribeMarshaler(b, topic.Name(), topts.Marshaler, so.Marshaler)

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
		// 再次解析：opts 可能在 wrapped 末尾覆盖 Marshaler
		final := &SubscribeOptions{}
		applySubscribeOptions(final, wrappedOpts...)
		dec := ResolveSubscribeMarshaler(b, topic.Name(), topts.Marshaler, final.Marshaler)
		ev, err := DecodeTyped[T](dec, raw)
		if err != nil {
			return fmt.Errorf("bus: unmarshal %q: %w", topic.Name(), err)
		}
		return h(ctx, ev)
	}, wrappedOpts...)
}

func publishRawTyped[T any](ctx context.Context, b Bus, topic Topic[T], raw *RawEvent, opts ...PublishOption) error {
	if raw == nil {
		return fmt.Errorf("bus: nil raw event")
	}
	// 转发：整份 RawEvent 交给 Bus，保留 ID/Key/Headers/Time；topic 参数覆盖逻辑名
	return b.Publish(ctx, topic.Name(), raw, opts...)
}
