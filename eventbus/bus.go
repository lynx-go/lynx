// package eventbus 提供 Lynx 一等消息总线：进程内状态协同与跨进程域事件的统一抽象。
//
// 设计原则：开箱即用（内存默认零配置）、配置开放（Topic 级覆盖）、扩展开放（Transport/Marshaler/Middleware）。
//
// 核心只有三个概念：Bus / Topic[T] / Event[T]，其余（Broker/Transport/RouteKey/Router）下沉为实现细节。
package eventbus

import (
	"context"
	"log/slog"
	"maps"
	"time"
)

// Bus 是应用级消息总线的核心接口，既是服务也是健康检查项。
// 任意 Service 可通过 AppContext.Bus() 取得当前总线实例。
//
// 投递语义由实现决定，两类实现是两个极端，订阅方必须按实现侧语义编写：
//   - 内存 Bus（默认）：at-most-once。订阅者缓冲满即丢弃事件（仅 Error
//     日志），handler 重试耗尽后丢弃；不反压发布者。适合进程内状态协同。
//   - 持久化 Bus（contrib/watermill-kafka 等）：at-least-once。处理失败
//     会被重投（Kafka 消费组语义），重复投递需业务幂等兜底。
//
// 同一逻辑 topic 的多个 handler 都收到每条消息（进程内扇出）；同一事件的
// handler 共享一条 transport 订阅。消费组 / 消费者成员数是后端配置
// （kafka consumer.group_id / instances），不进 Bus 层。
type Bus interface {
	// Publish 发布业务对象到逻辑 topic，按 Topic 的 Marshaler 序列化。
	// topic 为逻辑名，物理映射由 Bus 实现决定（内存直接投递，持久化 Bus 按配置路由）。
	// payload 为 []byte 时跳过序列化直发（原 Bus.PublishRaw 的能力）。
	Publish(ctx context.Context, topic string, payload any, opts ...PublishOption) error

	// Subscribe 订阅逻辑 topic；handler 名由 WithHandlerName 指定，为空时使用 topic，且在 Bus 内全局唯一。
	// 同一 topic 的多个 handler 均收到每条消息（进程内并行扇出）；同一事件
	// 共享一条 transport 订阅，消费组 / 成员数由后端配置决定。
	// 内存 Bus 允许 Start 后动态订阅；持久化 Bus 的 Start 前后语义由实现保证。
	Subscribe(ctx context.Context, topic string, h HandlerFunc, opts ...SubscribeOption) error

	// MarshalerFor 返回 topic 的序列化器（查找序：TopicMarshalers[t] →
	// Topics[t].Marshaler → 全局默认 → JSON）。
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
	ID      string            `json:"id"`
	Topic   string            `json:"topic"`
	Key     string            `json:"key"`
	Headers map[string]string `json:"headers"`
	Payload T                 `json:"payload"`
	Time    time.Time         `json:"time"`
}

// LogValue 实现 slog.LogValuer：事件信封以结构化 group 参与日志输出——
// 字段名与 JSON 标签一致、键序与结构体声明一致。订阅方直接
//
//	logger.InfoContext(ctx, "recv order created", "handler", name, "event", e)
//
// 即可：TextHandler 输出 event.id=... event.topic=... 的独立键值对，
// JSONHandler（含 contrib/zap 桥接）输出 "event":{...} 嵌套对象。相比直接
// 打印结构体（TextHandler 回落 fmt.Sprintf("%+v")：整段 Go 语法、字段不可
// 单独检索、map 顺序不定）或 json.Marshal 转字符串（被转义、JSONHandler 下
// 二次编码），结构化字段可被日志系统按 event.id / event.topic 过滤聚合。
//
// 接收者为指针：订阅 handler 持有的 *Event[T] 即生效，值类型 Event[T] 不
// 参与本渲染。LogValue 在 handler 真正写记录时才解析，级别未启用零开销；
// nil 指针安全（返回 "<nil>"，不 panic）；payload 不可序列化时由 handler
// 就地降级（如 JSON 输出 "!ERROR:..." 占位），不丢整条记录。headers 可能与
// ctx 传播属性重复、payload 可能大或敏感，调用方按需只记录子集。
func (e *Event[T]) LogValue() slog.Value {
	if e == nil {
		return slog.StringValue("<nil>")
	}
	return slog.GroupValue(
		slog.String("id", e.ID),
		slog.String("topic", e.Topic),
		slog.String("key", e.Key),
		slog.Any("headers", e.Headers),
		slog.Any("payload", e.Payload),
		slog.Time("time", e.Time),
	)
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

// WithMetadata 把整表写入消息头；克隆调用方 map——后续 WithMetadataField
// 不会反向污染调用方传入的映射。
func WithMetadata(md map[string]string) PublishOption {
	return publishOptionFunc(func(o *PublishOptions) {
		if md == nil {
			o.Metadata = nil
			return
		}
		o.Metadata = maps.Clone(md)
	})
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
	HandlerName string
	// AutoAck 订阅即确认：只调用一次、不重试，失败仅记日志。持久化后端上
	// 该 handler 的结果不参与整条消息的确认裁决（不会触发重投）——仅用于
	// 可容忍丢失的旁路事件。
	AutoAck bool
	// ContinueOnError 处理失败仍确认，不再重试/重投（丢弃语义，两种 Bus 一致）。
	ContinueOnError bool
	// MaxInFlight 是订阅级在途上限（未确认消息的并发上限；消息内多 handler
	// 仍并行）。0 = 后端默认（watermill 为 1）；负数 = 不限制。内存 Bus
	// 每 handler 串行处理，忽略本项。**不是调用者可传的订阅选项**：唯一写入
	// 路径是 Topic 默认值（WithTopicMaxInFlight）与 Options.Topics[t]，经
	// Resolver.ResolveSubscription 解析后由适配器消费（消费组 / 成员数是
	// 后端配置，不在此结构）。
	MaxInFlight int
	// HandlerTimeout 是 handler 单次尝试的执行上限（0 = 不限制；负值 =
	// 显式禁用），同样由 Topic 默认值（WithTopicHandlerTimeout）与
	// Options.Topics[t] 填充；解析见 Resolver.ResolveSubscription。
	HandlerTimeout time.Duration
	// Retry 是订阅级重试默认（高→低：本字段 > Options.Topics[t].Retry > Options.Retry）。
	// Topic[T] 会把 WithTopicRetry 作为本字段的基础值注入，调用方选项可覆盖。
	Retry *RetryOptions
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

// WithAutoAck 订阅即确认：只调用一次、不重试，错误仅记日志；持久化后端上
// 不参与整条消息的确认裁决（不会触发重投，见 SubscribeOptions.AutoAck）。
// 仅用于可容忍丢失的旁路事件。
func WithAutoAck() SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.AutoAck = true })
}

// WithContinueOnError 处理失败仍确认，不再重试/重投（丢弃语义，两种 Bus 一致）。
func WithContinueOnError() SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.ContinueOnError = true })
}

// withMaxInFlight 注入订阅级在途上限（Topic 默认值路径；不对外暴露——
// 调用者不能直接设置，见 SubscribeOptions.MaxInFlight）。
func withMaxInFlight(n int) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.MaxInFlight = n })
}

// withHandlerTimeout 注入 handler 单次尝试超时（同 withMaxInFlight）。
func withHandlerTimeout(d time.Duration) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { o.HandlerTimeout = d })
}

// WithSubscribeRetry 覆盖本次订阅的重试默认，优先级高于 Topic / Topics 配置 / 全局。
func WithSubscribeRetry(r RetryOptions) SubscribeOption {
	return subscribeOptionFunc(func(o *SubscribeOptions) { r2 := r; o.Retry = &r2 })
}

// busOverride 同时实现 PublishOption 与 SubscribeOption。
// 与 lynx.WithBus（应用构造）不同：本 Option 仅覆盖单次 Publish/Subscribe 的 Bus 解析。
type busOverride struct{ bus Bus }

func (o busOverride) applyPublish(p *PublishOptions)     { p.Bus = o.bus }
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
