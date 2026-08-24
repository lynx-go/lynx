// package eventbus 提供 Lynx 一等消息总线：进程内状态协同与跨进程域事件的统一抽象。
//
// 设计原则：开箱即用（内存默认零配置）、配置开放（Topic 级覆盖）、扩展开放（Transport/Marshaler/Middleware）。
//
// 核心只有三个概念：Bus / Topic[T] / Event[T]，其余（Broker/Transport/RouteKey/Router）下沉为实现细节。
package eventbus

import (
	"context"
	"log/slog"
	"time"
)

// Bus 是应用级消息总线的核心接口，既是服务也是健康检查项。
// 任意 Service 可通过 AppContext.Bus() 取得当前总线实例。
type Bus interface {
	// Publish 发布业务对象到逻辑 topic，按 Topic 的 Marshaler 序列化。
	// topic 为逻辑名，物理映射由 Bus 实现决定（内存直接投递，持久化 Bus 按配置路由）。
	Publish(ctx context.Context, topic string, payload any, opts ...PublishOption) error

	// PublishRaw 以原始字节发布，跳过序列化（用于已序列化的 *Event 透传）。
	PublishRaw(ctx context.Context, topic string, data []byte, opts ...PublishOption) error

	// Subscribe 订阅逻辑 topic；handler 名由 WithHandlerName 指定，为空时使用 topic，且在 Bus 内全局唯一。
	// 内存 Bus 允许 Start 后动态订阅；持久化 Bus 的 Start 前后语义由实现保证。
	Subscribe(ctx context.Context, topic string, h HandlerFunc, opts ...SubscribeOption) error

	// MarshalerFor 返回 topic 的序列化器（TopicMarshalers 命中则用之，否则回退默认）。
	MarshalerFor(topic string) Marshaler

	// Service / Checker 内嵌由实现显式声明，避免循环导入 lynx。
	Name() string
	Init(ctx InitContext) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	CheckHealth() error
}

// InitContext 是 Bus 初始化所需的最小上下文，解耦对 lynx.AppContext 的直接依赖，
// 避免 bus → lynx → bus 循环。实际传入为 lynx.AppContext（方法集兼容）。
type InitContext interface {
	Context() context.Context
	Logger(args ...any) *slog.Logger
}

// HandlerFunc 是原始事件处理函数，返回错误时按订阅选项重试或确认。
type HandlerFunc func(ctx context.Context, event *RawEvent) error

// RawEvent 是总线在 wire 层的原始事件，对应旧 pubsub.Message。
type RawEvent struct {
	ID      string
	Topic   string
	Key     string
	Headers map[string]string
	Payload []byte
	Time    time.Time
}

// Event 是类型化事件信封，Payload 为业务对象。
type Event[T any] struct {
	ID      string
	Topic   string
	Key     string
	Headers map[string]string
	Payload T
	Time    time.Time
}

// PublishOptions 是发布行为的配置项。
type PublishOptions struct {
	MessageKey string
	Metadata   map[string]string
	Marshaler  Marshaler
	// Bus 覆盖本次调用解析到的 Bus（仅 Topic 方法路径使用；Bus.Publish 忽略）。
	Bus Bus
}

// PublishOption 配置 PublishOptions。
type PublishOption interface {
	applyPublish(*PublishOptions)
}

type publishOptionFunc func(*PublishOptions)

func (f publishOptionFunc) applyPublish(o *PublishOptions) { f(o) }

// ApplyPublishOptions 应用发布选项（供 contrib Bus 实现使用）。
func ApplyPublishOptions(o *PublishOptions, opts ...PublishOption) {
	applyPublishOptions(o, opts...)
}

// WithMessageKey 设置消息 key（写入 wire 的 x-message-key，亦进入 Event.Key）。
func WithMessageKey(key string) PublishOption {
	return publishOptionFunc(func(o *PublishOptions) { o.MessageKey = key })
}

// WithMetadata 合并消息头。
func WithMetadata(md map[string]string) PublishOption {
	return publishOptionFunc(func(o *PublishOptions) { o.Metadata = md })
}

// WithMetadataField 添加单条消息头。
func WithMetadataField(k, v string) PublishOption {
	return publishOptionFunc(func(o *PublishOptions) {
		if o.Metadata == nil {
			o.Metadata = map[string]string{}
		}
		o.Metadata[k] = v
	})
}

// WithPublishMarshaler 覆盖本次发布的序列化器，优先级高于 Topic / Bus 默认。
func WithPublishMarshaler(m Marshaler) PublishOption {
	return publishOptionFunc(func(o *PublishOptions) { o.Marshaler = m })
}

// SubscribeOptions 是订阅行为的配置项。
type SubscribeOptions struct {
	// HandlerName 在 Bus 内全局唯一；为空时实现应回退为 topic。
	HandlerName     string
	AutoAck         bool
	ContinueOnError bool
	Group           string
	Instances       int
	Marshaler       Marshaler
	// Bus 覆盖本次调用解析到的 Bus（仅 Topic 方法路径使用；Bus.Subscribe 忽略）。
	Bus Bus
}

// SubscribeOption 配置 SubscribeOptions。
type SubscribeOption interface {
	applySubscribe(*SubscribeOptions)
}

type subscribeOptionFunc func(*SubscribeOptions)

func (f subscribeOptionFunc) applySubscribe(o *SubscribeOptions) { f(o) }

// ApplySubscribeOptions 应用订阅选项（供 contrib Bus 实现使用）。
func ApplySubscribeOptions(o *SubscribeOptions, opts ...SubscribeOption) {
	applySubscribeOptions(o, opts...)
}

// WithHandlerName 设置订阅 handler 名（Bus 内全局唯一）；省略时使用 topic。
func WithHandlerName(name string) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.HandlerName = name })
}

// WithAutoAck 订阅即确认，处理失败不影响 Ack。
func WithAutoAck() SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.AutoAck = true })
}

// WithContinueOnError 处理失败仍确认，不再重投。
func WithContinueOnError() SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.ContinueOnError = true })
}

// WithGroup 显式指定消费组，覆盖 Transport 默认。
func WithGroup(group string) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.Group = group })
}

// WithInstances 显式指定同组消费者成员数。
func WithInstances(n int) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.Instances = n })
}

// WithSubscribeMarshaler 覆盖本次订阅的序列化器，供解码与 Publish 对称。
func WithSubscribeMarshaler(m Marshaler) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.Marshaler = m })
}

// busOverride 同时实现 PublishOption 与 SubscribeOption。
// 与 lynx.WithBus（应用构造）不同：本 Option 仅覆盖单次 Publish/Subscribe 的 Bus 解析。
type busOverride struct{ bus Bus }

func (o busOverride) applyPublish(p *PublishOptions)   { p.Bus = o.bus }
func (o busOverride) applySubscribe(s *SubscribeOptions) { s.Bus = o.bus }

// WithBus 覆盖本次 Publish/Subscribe 解析到的 Bus（优先级最高）。
// 日常路径依赖 Context / Default，不必手传。
func WithBus(b Bus) interface {
	PublishOption
	SubscribeOption
} {
	return busOverride{bus: b}
}

func applyPublishOptions(o *PublishOptions, opts ...PublishOption) {
	for _, opt := range opts {
		if opt != nil {
			opt.applyPublish(o)
		}
	}
}

func applySubscribeOptions(o *SubscribeOptions, opts ...SubscribeOption) {
	for _, opt := range opts {
		if opt != nil {
			opt.applySubscribe(o)
		}
	}
}
