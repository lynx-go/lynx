package eventbus

import (
	"context"
	"fmt"
)

// Topic 是类型化主题：编译期绑定 Payload 类型，运行时携带订阅/发布默认值。
// 业务侧定义 var OrderCreated = eventbus.NewTopic[Order]("order.created") 后，
// Publish/Subscribe 均以 Topic 为锚点，Marshaler/Group/Retry 自动对齐。
type Topic[T any] struct {
	name string
	opts TopicOptions
}

// TopicOptions 是 Topic[T] 的订阅/发布默认值（Options 返回的只读视图）。
type TopicOptions struct {
	Group           string
	Instances       int
	AutoAck         bool
	ContinueOnError bool
	Retry           *RetryOptions
	Marshaler       Marshaler
}

// TopicOption 配置 Topic。
type TopicOption func(*TopicOptions)

// WithTopicGroup 覆盖消费组。
func WithTopicGroup(group string) TopicOption {
	return func(o *TopicOptions) { o.Group = group }
}

// WithTopicInstances 覆盖并发成员数。
func WithTopicInstances(n int) TopicOption {
	return func(o *TopicOptions) { o.Instances = n }
}

// WithTopicAutoAck 覆盖 AutoAck。
func WithTopicAutoAck() TopicOption {
	return func(o *TopicOptions) { o.AutoAck = true }
}

// WithTopicContinueOnError 覆盖 ContinueOnError。
func WithTopicContinueOnError() TopicOption {
	return func(o *TopicOptions) { o.ContinueOnError = true }
}

// WithTopicRetry 覆盖重试。
func WithTopicRetry(r RetryOptions) TopicOption {
	return func(o *TopicOptions) { r2 := r; o.Retry = &r2 }
}

// WithTopicMarshaler 覆盖序列化器。
func WithTopicMarshaler(m Marshaler) TopicOption {
	return func(o *TopicOptions) { o.Marshaler = m }
}

// NewTopic 创建类型化主题，name 为逻辑名（如 "order.created"）。
func NewTopic[T any](name string, opts ...TopicOption) Topic[T] {
	var o TopicOptions
	for _, fn := range opts {
		fn(&o)
	}
	return Topic[T]{name: name, opts: o}
}

// Name 返回逻辑主题名。
func (t Topic[T]) Name() string { return t.name }

// Options 返回主题选项的只读视图。
func (t Topic[T]) Options() TopicOptions { return t.opts }

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

// Subscribe 订阅类型化主题。Bus 解析同 Publish；handler 名见 WithHandlerName。
func (t Topic[T]) Subscribe(ctx context.Context, h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	so := &SubscribeOptions{}
	applySubscribeOptions(so, opts...)
	b, err := resolveBus(ctx, so.Bus)
	if err != nil {
		return err
	}
	return subscribeTyped(ctx, b, t, h, opts...)
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
func SubscribeTyped[T any](ctx context.Context, b Bus, topic Topic[T], h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	if b == nil {
		return ErrNoBus
	}
	return subscribeTyped(ctx, b, topic, h, opts...)
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

func subscribeTyped[T any](ctx context.Context, b Bus, topic Topic[T], h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
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

	// CORE-03：wrappedOpts 在订阅时已固定，Marshaler 解析提升到订阅时
	// 一次完成、闭包直接捕获——此前每条消息重复 apply+解析纯属分配浪费。
	// 优先级语义不变：用户 opts 排在 wrappedOpts 末尾最后生效，可在
	// 订阅时覆盖 Topic 默认 Marshaler。
	final := &SubscribeOptions{}
	applySubscribeOptions(final, wrappedOpts...)
	dec := ResolveSubscribeMarshaler(b, topic.Name(), topts.Marshaler, final.Marshaler)

	return b.Subscribe(ctx, topic.Name(), func(ctx context.Context, raw *RawEvent) error {
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
