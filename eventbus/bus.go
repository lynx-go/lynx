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

	// Subscribe 订阅逻辑 topic，handlerName 在 Bus 内全局唯一。
	// 内存 Bus 允许 Start 后动态订阅；持久化 Bus 的 Start 前后语义由实现保证。
	Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error

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
}

// PublishOption 配置 PublishOptions。
type PublishOption func(*PublishOptions)

// WithMessageKey 设置消息 key（写入 wire 的 x-message-key，亦进入 Event.Key）。
func WithMessageKey(key string) PublishOption {
	return func(o *PublishOptions) { o.MessageKey = key }
}

// WithMetadata 合并消息头。
func WithMetadata(md map[string]string) PublishOption {
	return func(o *PublishOptions) { o.Metadata = md }
}

// WithMetadataField 添加单条消息头。
func WithMetadataField(k, v string) PublishOption {
	return func(o *PublishOptions) {
		if o.Metadata == nil {
			o.Metadata = map[string]string{}
		}
		o.Metadata[k] = v
	}
}

// SubscribeOptions 是订阅行为的配置项。
type SubscribeOptions struct {
	AutoAck         bool
	ContinueOnError bool
	Group           string
	Instances       int
}

// SubscribeOption 配置 SubscribeOptions。
type SubscribeOption func(*SubscribeOptions)

// WithAutoAck 订阅即确认，处理失败不影响 Ack。
func WithAutoAck() SubscribeOption { return func(o *SubscribeOptions) { o.AutoAck = true } }

// WithContinueOnError 处理失败仍确认，不再重投。
func WithContinueOnError() SubscribeOption { return func(o *SubscribeOptions) { o.ContinueOnError = true } }

// WithGroup 显式指定消费组，覆盖 Transport 默认。
func WithGroup(group string) SubscribeOption { return func(o *SubscribeOptions) { o.Group = group } }

// WithInstances 显式指定同组消费者成员数。
func WithInstances(n int) SubscribeOption { return func(o *SubscribeOptions) { o.Instances = n } }
