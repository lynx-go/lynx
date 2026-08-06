// Package pubsub 提供基于 Watermill 的消息发布订阅抽象：
// Broker 门面、Transport 后端与消息 Handler。
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

// Broker 是消息代理门面组件：按 topic 路由到 Transport，统一发布订阅。
type Broker interface {
	lynx.ServerLike
	// Publish 发布消息到逻辑 topic；路由表未命中时走默认 Transport。
	// payload 为 *Message 时直接发送（字节级语义，不序列化）；
	// 否则视为业务对象，经 Broker 的 Marshaler 自动序列化后发送。
	Publish(ctx context.Context, topic string, payload any, opts ...PublishOption) error
	// Subscribe 注册 topic 的消费 handler。Start 前调用为缓冲注册，
	// Start 后调用返回错误。
	Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
	// Route 显式将 topic 路由到指定 Transport，覆盖自动路由；须在 Start 前调用。
	// transport 侧主题名与逻辑 topic 同名；需要别名时用 RouteKey。
	Route(topic string, t Transport)
	// RouteKey 显式将逻辑 topic 路由到指定 Transport，并以 key 作为调用
	// transport 时的主题名（如 kafka Transport 的 kafka 段配置 key）；
	// 覆盖自动路由，须在 Start 前调用。
	RouteKey(topic string, t Transport, key string)
	// MarshalerFor 返回 topic 的业务对象序列化器：TopicMarshalers 命中则
	// 用之，否则回退 Options.Marshaler 或 JSON 默认。
	MarshalerFor(topic string) Marshaler
}

// RetryOptions 配置 handler 处理失败后的重试行为。
type RetryOptions struct {
	// MaxRetries 是最大重试次数，0 表示不重试。
	MaxRetries int
	// Backoff 是每次重试的固定间隔，0 表示不等待。
	Backoff time.Duration
}

// Options 是 Broker 的配置项。
type Options struct {
	// Transports 参与自动路由：每个 Transport.Topics() 声明的 topic
	// 自动路由到该 Transport；重复声明同一 topic 时 Init 报错。
	Transports []Transport
	// DefaultTransport 承接路由表未命中的 topic。
	DefaultTransport Transport
	// Retry 配置 handler 失败重试；nil 时使用默认 {MaxRetries: 3}。
	// 注意：重试耗尽后消息不确认，依赖 at-least-once 语义的 Transport
	// （如 Kafka 关闭自动提交）会重投；开启自动提交时 offset 已被提交，
	// 消息可能静默丢失，需自行权衡。
	Retry *RetryOptions
	// Marshaler 负责业务对象与 Payload 的序列化（Publish 传业务对象与
	// Subscribe[T] 使用）；nil 时使用 JSON 默认。
	Marshaler Marshaler
	// TopicMarshalers 按逻辑 topic 覆盖 Marshaler；未命中时回退
	// Marshaler（或 JSON 默认）。同一 topic 的发布与消费必须使用
	// 同一种格式，跨服务部署时需对齐配置。
	TopicMarshalers map[string]Marshaler
}

// NewBroker 创建消息代理门面。
func NewBroker(opts Options) Broker {
	return &broker{
		options:  opts,
		routes:   map[string]routeEntry{},
		explicit: map[string]routeEntry{},
	}
}

// HandlerFunc 是事件处理函数，返回错误时按订阅选项决定重试或确认。
type HandlerFunc func(ctx context.Context, event *Message) error

// Handler 定义事件处理器的元信息与处理函数。
type Handler interface {
	EventName() string
	HandlerName() string
	HandlerFunc() HandlerFunc
}

// HandlerOptions 可为 Handler 附加订阅选项。
type HandlerOptions interface {
	Options() []SubscribeOption
}

// SubscribeOptions 是订阅行为的配置项。
type SubscribeOptions struct {
	AutoAck         bool   `json:"auto_ack"`
	ContinueOnError bool   `json:"continue_on_error"`
	Group           string `json:"group"`
	Instances       int    `json:"instances"`
}

// SubscribeOption 用于配置 SubscribeOptions 的选项函数。
type SubscribeOption func(*SubscribeOptions)

// WithAutoAck 设置订阅为自动确认：消息到达即 Ack，处理失败不影响确认。
func WithAutoAck() SubscribeOption {
	return func(opts *SubscribeOptions) { opts.AutoAck = true }
}

// WithContinueOnError 设置处理失败时仍确认消息，不再重投。
func WithContinueOnError() SubscribeOption {
	return func(opts *SubscribeOptions) { opts.ContinueOnError = true }
}

// WithGroup 显式指定消费组，覆盖 Transport 配置的默认组。
func WithGroup(group string) SubscribeOption {
	return func(opts *SubscribeOptions) { opts.Group = group }
}

// WithInstances 显式指定同组消费者成员数，覆盖 Transport 配置的默认值。
func WithInstances(n int) SubscribeOption {
	return func(opts *SubscribeOptions) { opts.Instances = n }
}

// PublishOptions 是发布行为的配置项。
type PublishOptions struct {
	MessageKey string            `json:"message_key"`
	Metadata   map[string]string `json:"metadata"`
}

// PublishOption 用于配置 PublishOptions 的选项函数。
type PublishOption func(*PublishOptions)

// WithMessageKey 设置消息 key，发布时写入消息 Key 字段。
func WithMessageKey(key string) PublishOption {
	return func(opts *PublishOptions) { opts.MessageKey = key }
}

// WithMetadata 设置消息元数据，发布时合并进消息头。
func WithMetadata(metadata map[string]string) PublishOption {
	return func(opts *PublishOptions) { opts.Metadata = metadata }
}

// WithMetadataField 添加单条消息元数据字段。
func WithMetadataField(key, value string) PublishOption {
	return func(opts *PublishOptions) {
		if opts.Metadata == nil {
			opts.Metadata = map[string]string{}
		}
		opts.Metadata[key] = value
	}
}

type pendingSubscription struct {
	topic       string
	handlerName string
	handler     HandlerFunc
	opts        SubscribeOptions
}

// subscriberAdapter 将 Transport 适配为 watermill 的 Subscriber。
type subscriberAdapter struct {
	t    Transport
	opts SubscriptionOptions
}

func (a subscriberAdapter) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	return a.t.Subscribe(ctx, topic, a.opts)
}

// Close 是 no-op：Transport 生命周期由应用统一管理（broker.Stop 不关闭
// Transport），这里不再委托 Transport.Stop，否则多 broker 共享同一
// Transport 时，一个 broker 关闭会杀死其他 broker 的客户端。订阅 channel
// 在订阅 ctx 取消时自行关闭，无需在此处理。
func (a subscriberAdapter) Close() error { return nil }

// Broker 是 Broker 接口的具体实现。
type broker struct {
	options Options
	app     lynx.App
	router  *message.Router

	// routes 与 explicit 由 routeMu 保护：Route/RouteKey 与 Init 自动路由写，
	// resolve（Publish/Start）读。
	routeMu  sync.RWMutex
	routes   map[string]routeEntry
	explicit map[string]routeEntry

	mu      sync.Mutex
	pending []pendingSubscription
	started bool
}

// routeEntry 是路由表的一项：逻辑 topic → (Transport, transport 侧主题名)。
// key 为空时表示与逻辑 topic 同名（Route 与自动路由的缺省语义）。
type routeEntry struct {
	t   Transport
	key string
}

// Name 返回组件名称 "pubsub-broker"。
func (b *broker) Name() string { return "pubsub-broker" }

// Route 显式将 topic 路由到指定 Transport，覆盖自动路由；须在 Start 前调用。
// transport 侧主题名与逻辑 topic 同名；需要别名时用 RouteKey。
func (b *broker) Route(topic string, t Transport) {
	b.RouteKey(topic, t, topic)
}

// RouteKey 显式将逻辑 topic 路由到指定 Transport，并以 key 作为调用
// transport 时的主题名（如 kafka Transport 的 kafka 段配置 key）；
// 覆盖自动路由，须在 Start 前调用。
func (b *broker) RouteKey(topic string, t Transport, key string) {
	b.routeMu.Lock()
	defer b.routeMu.Unlock()
	b.routes[topic] = routeEntry{t: t, key: key}
	b.explicit[topic] = routeEntry{t: t, key: key}
}

// CheckHealth 报告 Broker 是否在运行。
func (b *broker) CheckHealth() error {
	if b.router == nil {
		return errors.New("broker is not initialized")
	}
	if b.router.IsRunning() {
		return nil
	}
	return errors.New("broker is not running")
}

// Init 创建 watermill router 并执行自动路由。
func (b *broker) Init(app lynx.App) error {
	b.app = app
	slogger := app.Logger("component", "pubsub")
	logger := watermill.NewSlogLogger(slogger)

	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return err
	}
	retry := middleware.Retry{MaxRetries: 3}
	if b.options.Retry != nil {
		retry.MaxRetries = b.options.Retry.MaxRetries
		if b.options.Retry.Backoff > 0 {
			retry.InitialInterval = b.options.Retry.Backoff
			retry.MaxInterval = b.options.Retry.Backoff
		}
	}
	router.AddMiddleware(
		middleware.Recoverer,
		middleware.CorrelationID,
		retry.Middleware,
	)
	router.AddPlugin(plugin.SignalsHandler)
	b.router = router

	b.routeMu.Lock()
	defer b.routeMu.Unlock()
	for _, t := range b.options.Transports {
		for _, topic := range t.Topics() {
			if _, ok := b.explicit[topic]; ok {
				continue // 显式 Route 覆盖自动路由，不检查不报错
			}
			if prev, ok := b.routes[topic]; ok && prev.t != t {
				return fmt.Errorf("topic %q is routed to multiple transports", topic)
			}
			b.routes[topic] = routeEntry{t: t, key: topic}
		}
	}
	return nil
}

// resolve 返回 topic 的 (Transport, transport 侧主题名)。
// 显式/自动路由命中时使用路由登记的 key（RouteKey 别名）；
// 未命中回退 DefaultTransport，主题名与逻辑 topic 同名。
func (b *broker) resolve(topic string) (Transport, string, error) {
	b.routeMu.RLock()
	r, ok := b.routes[topic]
	b.routeMu.RUnlock()
	if ok {
		key := r.key
		if key == "" {
			key = topic
		}
		return r.t, key, nil
	}
	if b.options.DefaultTransport != nil {
		return b.options.DefaultTransport, topic, nil
	}
	return nil, "", fmt.Errorf("no transport routed for topic %q", topic)
}

// Start 将缓冲订阅统一注册进 watermill router 并运行。
// 两阶段提交：先预校验全部订阅（路由归属 + handler 重名），全部通过后才
// 注册任何 handler——部分失败不会留下已注册的残留，补充 Route 后重试
// Start 安全，也不会触发 watermill 的 DuplicateHandlerNameError panic。
func (b *broker) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return errors.New("broker already started")
	}
	if b.router == nil {
		b.mu.Unlock()
		return errors.New("broker is not initialized")
	}
	type registration struct {
		topic       string
		handlerName string
		adapter     subscriberAdapter
		handler     message.NoPublishHandlerFunc
	}
	registrations := make([]registration, 0, len(b.pending))
	names := make(map[string]struct{}, len(b.pending))
	for _, p := range b.pending {
		if _, dup := names[p.handlerName]; dup {
			b.mu.Unlock()
			return fmt.Errorf("duplicate handler name %q", p.handlerName)
		}
		names[p.handlerName] = struct{}{}
		t, key, err := b.resolve(p.topic)
		if err != nil {
			b.mu.Unlock()
			return err
		}
		registrations = append(registrations, registration{
			// watermill 订阅 topic 必须是 transport 侧主题名（RouteKey 别名），
			// 与 Publish 侧的 key 翻译保持一致。
			topic:       key,
			handlerName: p.handlerName,
			adapter: subscriberAdapter{
				t:    t,
				opts: SubscriptionOptions{Group: p.opts.Group, Instances: p.opts.Instances},
			},
			handler: b.wrapHandler(p.handler, p.opts),
		})
	}
	// 阶段 2：注册。AddConsumerHandler 不返回错误（重名直接 panic），
	// 阶段 1 的预校验已排除该路径，注册在此不可失败。
	for _, r := range registrations {
		b.router.AddConsumerHandler(r.handlerName, r.topic, r.adapter, r.handler)
	}
	b.started = true
	b.pending = nil
	// 先释放锁再 Run：Run 阻塞至停止，期间 Subscribe 等取锁方法必须可用
	//（返回 "already started" 错误而非死等）。
	b.mu.Unlock()

	return b.router.Run(ctx)
}

// Stop 关闭 watermill router。
func (b *broker) Stop(ctx context.Context) {
	if b.router != nil {
		if err := b.router.Close(); err != nil {
			log.ErrorContext(ctx, "error closing router", err)
		}
	}
}

// Subscribe 缓冲注册订阅；Start 后调用返回错误。handlerName 在缓冲期内
// 必须唯一（watermill 的 AddConsumerHandler 对重名直接 panic，这里提前报错）。
func (b *broker) Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error {
	o := &SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	if handlerName == "" {
		return errors.New("handler name is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return errors.New("cannot subscribe to a started broker")
	}
	for _, p := range b.pending {
		if p.handlerName == handlerName {
			return fmt.Errorf("duplicate handler name %q", handlerName)
		}
	}
	b.pending = append(b.pending, pendingSubscription{
		topic: topic, handlerName: handlerName, handler: h, opts: *o,
	})
	return nil
}

// wrapHandler 包装用户 handler：注入消息 ID/key 上下文，统一 Ack 语义。
func (b *broker) wrapHandler(h HandlerFunc, o SubscribeOptions) message.NoPublishHandlerFunc {
	handler := func(msg *message.Message) error {
		ctx := ContextWithMessageID(msg.Context(), msg.UUID)
		ctx = ContextWithMessageKey(ctx, msg.Metadata.Get(MessageKeyKey.String()))
		ctx = log.Context(ctx, log.FromContext(ctx), MessageIDKey.String(), msg.UUID)

		if err := h(ctx, fromWatermill(msg)); err != nil {
			log.ErrorContext(ctx, "error handling message", err, "x-message-id", msg.UUID)
			if o.ContinueOnError {
				msg.Ack()
				return nil
			}
			return err
		}
		msg.Ack()
		return nil
	}
	if o.AutoAck {
		// AutoAck 语义：先确认再执行，最多执行一次。handler 出错仅记日志
		// （wrapHandler 内已记录），返回 nil 以免触发 Retry 中间件重试。
		return func(msg *message.Message) error {
			msg.Ack()
			_ = handler(msg)
			return nil
		}
	}
	return handler
}

// cloneMessage 浅拷贝 Message 并深拷贝 Headers，发布时只修改克隆体，
// 避免 Publish 选项就地改写调用方的消息（同一消息并发发布会竞态）。
func cloneMessage(m *Message) *Message {
	cp := *m
	if m.Headers != nil {
		cp.Headers = make(map[string]string, len(m.Headers))
		for k, v := range m.Headers {
			cp.Headers[k] = v
		}
	}
	return &cp
}

// Publish 发布消息到逻辑 topic；路由未命中且无默认 Transport 时返回错误。
// payload 为 *Message 时直接发送（字节级语义，不序列化）；否则视为业务
// 对象，经 Marshaler 序列化后发送。
func (b *broker) Publish(ctx context.Context, topic string, payload any, opts ...PublishOption) error {
	o := &PublishOptions{}
	for _, opt := range opts {
		opt(o)
	}
	var m *Message
	if msg, ok := payload.(*Message); ok {
		// 类型断言命中的 typed nil 会在 cloneMessage 中解引用 panic（P2-5）。
		if msg == nil {
			return errors.New("pubsub: payload is a typed nil *Message")
		}
		m = msg
	} else {
		data, err := b.MarshalerFor(topic).Marshal(payload)
		if err != nil {
			return fmt.Errorf("pubsub: marshal payload: %w", err)
		}
		m = NewMessage(data)
	}
	m = cloneMessage(m)
	if o.MessageKey != "" {
		m.Key = o.MessageKey
	}
	for k, v := range o.Metadata {
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		m.Headers[k] = v
	}
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	// 以 transport 侧主题名调用 transport（RouteKey 别名）；缺省与逻辑 topic 同名。
	return t.Publish(ctx, key, toWatermill(m))
}

// MarshalerFor 返回 topic 的业务对象序列化器：TopicMarshalers 命中则用之，
// 否则回退 Options.Marshaler 或 JSON 默认。
func (b *broker) MarshalerFor(topic string) Marshaler {
	if m, ok := b.options.TopicMarshalers[topic]; ok {
		return m
	}
	if b.options.Marshaler != nil {
		return b.options.Marshaler
	}
	return JSONMarshaler{}
}
