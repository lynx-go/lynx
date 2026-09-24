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
// 消费组 / 消费者成员数是后端配置（kafka consumer.group_id / instances），
// 不在 Topic 上声明。
type TopicOptions struct {
	MaxInFlight     int
	AutoAck         bool
	ContinueOnError bool
	Retry           *RetryOptions
	Marshaler       Marshaler
}

// TopicOption 配置 Topic。
type TopicOption func(*TopicOptions)

// WithTopicMaxInFlight 设置订阅级在途上限：同一事件订阅内"未确认消息"的
// 并发上限（消息内多 handler 仍并行）。0 = 后端默认（watermill 为 1，串行
// 并恢复同订阅处理顺序）；负数 = 不限制（逃生口，不推荐）。持久化后端专用：
// 内存 Bus 每 handler 串行处理，无此概念。
func WithTopicMaxInFlight(n int) TopicOption {
	return func(o *TopicOptions) { o.MaxInFlight = n }
}

// WithTopicAutoAck 覆盖 AutoAck。
func WithTopicAutoAck() TopicOption {
	return func(o *TopicOptions) { o.AutoAck = true }
}

// WithTopicContinueOnError 覆盖 ContinueOnError。
func WithTopicContinueOnError() TopicOption {
	return func(o *TopicOptions) { o.ContinueOnError = true }
}

// WithTopicRetry 覆盖重试（经 subscribeTyped 转发为订阅基础值，调用方 WithSubscribeRetry 可覆盖）。
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
// 原始载荷透传：payload 为 *RawEvent 时整份信封转发（保留 ID/Key/Headers/
// Time，逻辑名以 Topic 为准）；为 []byte 时跳过序列化直发——两者都不经过
// Topic/Bus 级 marshaler（见 BuildRawEvent 的类型分支）。
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

func publishTyped[T any](ctx context.Context, b Bus, topic Topic[T], payload T, opts ...PublishOption) error {
	// Topic Marshaler 作为较低优先级默认：先注入，再让调用方 opts 覆盖
	var base []PublishOption
	if m := topic.Options().Marshaler; m != nil {
		base = append(base, WithPublishMarshaler(m))
	}
	base = append(base, opts...)
	return b.Publish(ctx, topic.Name(), payload, base...)
}

func subscribeTyped[T any](ctx context.Context, b Bus, topic Topic[T], h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error {
	topts := topic.Options()
	// CORE-03：解码器在订阅时一次解析、闭包直接捕获。订阅侧无调用级覆盖
	// （有意的不对称，见 docs/design-eventbus.md §5.2）：同一 topic 的 wire
	// 格式由发布侧决定，解码按 Topic 携带 > Bus 级（MarshalerFor）。
	dec := ResolveMarshaler(b, topic.Name(), topts.Marshaler, nil)

	// Topic 默认值作为基础项注入，调用方 opts 排在末尾最后生效
	wrappedOpts := make([]SubscribeOption, 0, len(opts)+4)
	if topts.MaxInFlight != 0 {
		wrappedOpts = append(wrappedOpts, withMaxInFlight(topts.MaxInFlight))
	}
	if topts.AutoAck {
		wrappedOpts = append(wrappedOpts, WithAutoAck())
	}
	if topts.ContinueOnError {
		wrappedOpts = append(wrappedOpts, WithContinueOnError())
	}
	if topts.Retry != nil {
		wrappedOpts = append(wrappedOpts, WithSubscribeRetry(*topts.Retry))
	}
	wrappedOpts = append(wrappedOpts, opts...)

	return b.Subscribe(ctx, topic.Name(), func(ctx context.Context, raw *RawEvent) error {
		ev, err := DecodeTyped[T](dec, raw)
		if err != nil {
			return fmt.Errorf("bus: unmarshal %q: %w", topic.Name(), err)
		}
		return h(ctx, ev)
	}, wrappedOpts...)
}
