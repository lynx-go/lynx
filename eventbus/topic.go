package eventbus

// Topic 是类型化主题：编译期绑定 Payload 类型，运行时携带订阅/发布默认值。
// 业务侧定义 var OrderCreated = eventbus.NewTopic[Order]("order.created") 后，
// Publish/Subscribe 均以 Topic 为锚点，Marshaler/Group/Retry 自动对齐。
type Topic[T any] struct {
	name string
	opts topicOptions
}

type topicOptions struct {
	Group           string
	Instances       int
	AutoAck         bool
	ContinueOnError bool
	Retry           *RetryOptions
	Marshaler       Marshaler
}

// TopicOption 配置 Topic。
type TopicOption func(*topicOptions)

// WithTopicGroup 覆盖消费组。
func WithTopicGroup(group string) TopicOption { return func(o *topicOptions) { o.Group = group } }

// WithTopicInstances 覆盖并发成员数。
func WithTopicInstances(n int) TopicOption { return func(o *topicOptions) { o.Instances = n } }

// WithTopicAutoAck 覆盖 AutoAck。
func WithTopicAutoAck() TopicOption { return func(o *topicOptions) { o.AutoAck = true } }

// WithTopicContinueOnError 覆盖 ContinueOnError。
func WithTopicContinueOnError() TopicOption { return func(o *topicOptions) { o.ContinueOnError = true } }

// WithTopicRetry 覆盖重试。
func WithTopicRetry(r RetryOptions) TopicOption { return func(o *topicOptions) { r2 := r; o.Retry = &r2 } }

// WithTopicMarshaler 覆盖序列化器。
func WithTopicMarshaler(m Marshaler) TopicOption { return func(o *topicOptions) { o.Marshaler = m } }

// NewTopic 创建类型化主题，name 为逻辑名（如 "order.created"）。
func NewTopic[T any](name string, opts ...TopicOption) Topic[T] {
	var o topicOptions
	for _, fn := range opts {
		fn(&o)
	}
	return Topic[T]{name: name, opts: o}
}

// Name 返回逻辑主题名。
func (t Topic[T]) Name() string { return t.name }

// Options 返回主题选项的只读视图（内部使用）。
func (t Topic[T]) Options() topicOptions { return t.opts }
