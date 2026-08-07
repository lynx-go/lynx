// Package pubsub 提供基于 Watermill 的消息发布订阅抽象：
// Broker 门面、Transport 后端与消息 Handler。
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
	"github.com/lynx-go/lynx"
)

// Broker 是消息代理门面服务：按 topic 路由到 Transport，统一发布订阅。
type Broker interface {
	lynx.Service
	lynx.Checker
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

// LogMessageOptions 控制消息收发日志（debug 级），发布与订阅两侧独立。
type LogMessageOptions struct {
	// Publish 对发布输出 "publishing message" 日志。
	Publish bool
	// Subscribe 对消费输出 "received message" 日志。
	Subscribe bool
}

// EventOptions 是单事件（逻辑 topic）的选项。Subscribe 时合并为默认订阅
// 选项（显式传入的 SubscribeOption 优先）；LogMessage 覆盖 Broker 级收发
// 日志配置（nil = 沿用全局）；Retry 覆盖 Broker 级重试（nil = 沿用全局）。
// NewFromConfig 从配置 pubsub.events 段加载；直接使用 NewBroker 的用户也可
// 手动配置。
type EventOptions struct {
	// LogMessage 覆盖该事件的收发日志配置；nil = 沿用 Options.LogMessage。
	// 注意：事件级是整体覆盖（非逐字段合并），未开启的一侧关闭。
	LogMessage *LogMessageOptions
	// AutoAck 作为 Subscribe 默认选项：消息到达即 Ack，处理失败不影响确认。
	AutoAck bool
	// ContinueOnError 作为 Subscribe 默认选项：处理失败时仍确认消息。
	ContinueOnError bool
	// Group 作为 Subscribe 默认消费组，覆盖 Transport 配置的默认组。
	Group string
	// Instances 作为 Subscribe 默认同组消费者成员数，0 = 沿用 Transport 默认。
	Instances int
	// Retry 覆盖 Broker 级重试；nil = 沿用 Options.Retry（缺省 {MaxRetries: 3}）。
	Retry *RetryOptions
}

// Options 是 Broker 的配置项。
type Options struct {
	// Transports 参与自动路由：每个 Transport.Topics() 声明的 topic
	// 自动路由到该 Transport；重复声明同一 topic 时 Init 报错。
	Transports []Transport
	// DefaultTransport 承接路由表未命中的 topic。
	DefaultTransport Transport
	// LogMessage 是全局收发日志配置（所有事件未单独配置时的默认值）；
	// nil = 默认不输出。事件可通过 Events[topic].LogMessage 覆盖。
	LogMessage *LogMessageOptions
	// Debug 控制 watermill 核心（router）debug 日志的输出：true 时按应用
	// 日志级别输出（仅当应用处于 debug 级别时可见）；false（缺省）时
	// watermill 核心 debug 日志一律不输出（过滤为 info+），避免订阅接线、
	// 中间件装载等内部日志刷屏。不作用于 transport 自己的日志。
	Debug bool
	// Retry 配置 handler 失败重试（所有事件的默认值）；nil 时使用默认
	// {MaxRetries: 3}。事件可通过 Events[topic].Retry 覆盖。
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
	// Events 按逻辑 topic 配置事件级选项：Subscribe 时合并为默认选项
	//（显式 SubscribeOption 优先），收发日志/重试按事件覆盖全局配置。
	// NewFromConfig 从配置 pubsub.events 段加载。
	Events map[string]EventOptions
}

// NewBroker 创建消息代理门面。
func NewBroker(opts Options) Broker {
	return &broker{
		options:  opts,
		routes:   map[string]routeEntry{},
		explicit: map[string]routeEntry{},
		logger:   slog.Default(),
	}
}

// HandlerFunc 是事件处理函数，返回错误时按订阅选项决定重试或确认。
type HandlerFunc func(ctx context.Context, event *Message) error

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
	// logger 是服务日志实例：Init(ctx) 时从 ctx.Logger 取，未 Init 时
	// 回落 slog.Default()。
	logger *slog.Logger
	router *message.Router

	// routes 与 explicit 由 routeMu 保护：Route/RouteKey 与 Init 自动路由写，
	// resolve（Publish/Start）读。
	routeMu  sync.RWMutex
	routes   map[string]routeEntry
	explicit map[string]routeEntry

	mu      sync.Mutex
	pending []pendingSubscription
	started bool
	// stopped 标记真实运行（Start 已置位 started）后的 Stop：此时生命周期
	// 已结束，Subscribe/Start 返回明确的 stopped 错误而非误导的 started
	// 文案。Stop-before-Start（失败清理路径）不置位，不破坏后续正常
	// Start/Stop 流程（回归 TestBrokerStopBeforeStart）。
	stopped bool
}

// routeEntry 是路由表的一项：逻辑 topic → (Transport, transport 侧主题名)。
// key 为空时表示与逻辑 topic 同名（Route 与自动路由的缺省语义）。
type routeEntry struct {
	t   Transport
	key string
}

// Name 返回服务名称 "pubsub-broker"。
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
func (b *broker) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		b.logger = ctx.Logger("service", "pubsub")
	}
	// Debug 关闭（缺省）时把 watermill 核心日志过滤为 info+：订阅接线、
	// 中间件装载等内部 debug 日志不输出，避免与业务 debug 日志混刷屏。
	wmLogger := b.logger
	if !b.options.Debug {
		wmLogger = slog.New(levelFilterHandler{level: slog.LevelInfo, h: b.logger.Handler()})
	}
	logger := watermill.NewSlogLogger(wmLogger)

	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return err
	}
	// 重试不再挂全局中间件：改按 handler 挂载（Start 期 Handler.AddMiddleware），
	// 事件级重试配置（事件配置 > Options.Retry > 默认 {3, 0}）才能生效，
	// 缺省行为与全局 Retry 等价。
	router.AddMiddleware(
		middleware.Recoverer,
		middleware.CorrelationID,
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
	if b.stopped {
		b.mu.Unlock()
		return errors.New("broker already stopped")
	}
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
			handler: b.wrapHandler(p.topic, p.handler, p.opts),
		})
	}
	// 阶段 2：注册。AddConsumerHandler 不返回错误（重名直接 panic），
	// 阶段 1 的预校验已排除该路径，注册在此不可失败。
	// 重试中间件按 handler 挂载（watermill 的 Handler.AddMiddleware），
	// 事件级配置生效；不再使用全局 Retry 中间件。
	for _, r := range registrations {
		h := b.router.AddConsumerHandler(r.handlerName, r.topic, r.adapter, r.handler)
		if retry, ok := b.retryMiddleware(r.topic); ok {
			h.AddMiddleware(retry)
		}
	}
	b.started = true
	b.pending = nil
	// 先释放锁再 Run：Run 阻塞至停止，期间 Subscribe 等取锁方法必须可用
	//（返回 "already started" 错误而非死等）。
	b.mu.Unlock()

	return b.router.Run(ctx)
}

// Stop 关闭 watermill router；关闭错误返回。
func (b *broker) Stop(ctx context.Context) error {
	b.mu.Lock()
	// 仅真实运行后的 Stop 标记 stopped（生命周期终结）；Stop-before-Start
	// 是失败清理路径，必须容忍且不改变后续 Start/Stop 语义。
	if b.started {
		b.stopped = true
	}
	b.mu.Unlock()
	if b.router != nil {
		if err := b.router.Close(); err != nil {
			b.logger.ErrorContext(ctx, "error closing router", "error", err)
			return err
		}
	}
	return nil
}

// Subscribe 缓冲注册订阅；Start 后调用返回错误。handlerName 在缓冲期内
// 必须唯一（watermill 的 AddConsumerHandler 对重名直接 panic，这里提前报错）。
// 事件级选项（Options.Events[topic]）合并为默认值，显式 SubscribeOption 优先。
func (b *broker) Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error {
	o := &SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	ev := b.eventOptions(topic)
	if !o.AutoAck {
		o.AutoAck = ev.AutoAck
	}
	if !o.ContinueOnError {
		o.ContinueOnError = ev.ContinueOnError
	}
	if o.Group == "" {
		o.Group = ev.Group
	}
	if o.Instances == 0 {
		o.Instances = ev.Instances
	}
	if handlerName == "" {
		return errors.New("handler name is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return errors.New("cannot subscribe to a stopped broker")
	}
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

// levelFilterHandler 丢弃低于 level 的日志记录，其余委托给 h。
// 用于把 watermill 核心日志过滤为 info+（Options.Debug 关闭时）。
type levelFilterHandler struct {
	level slog.Level
	h     slog.Handler
}

func (f levelFilterHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.level && f.h.Enabled(ctx, l)
}

func (f levelFilterHandler) Handle(ctx context.Context, r slog.Record) error {
	return f.h.Handle(ctx, r)
}

func (f levelFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelFilterHandler{f.level, f.h.WithAttrs(attrs)}
}

func (f levelFilterHandler) WithGroup(name string) slog.Handler {
	return levelFilterHandler{f.level, f.h.WithGroup(name)}
}

// eventOptions 返回 topic 的事件级选项（未配置时返回零值）。
func (b *broker) eventOptions(topic string) EventOptions {
	if ev, ok := b.options.Events[topic]; ok {
		return ev
	}
	return EventOptions{}
}

// logMessageFor 解析 topic 的收发日志配置：事件级整体覆盖 > 全局默认 > 关闭。
func (b *broker) logMessageFor(topic string) LogMessageOptions {
	if ev, ok := b.options.Events[topic]; ok && ev.LogMessage != nil {
		return *ev.LogMessage
	}
	if b.options.LogMessage != nil {
		return *b.options.LogMessage
	}
	return LogMessageOptions{}
}

// wrapHandler 包装用户 handler：注入消息 ID/key 上下文，按事件配置输出
// 收发日志，统一 Ack 语义。重试由 per-handler 中间件负责（Start 期挂载）。
func (b *broker) wrapHandler(topic string, h HandlerFunc, o SubscribeOptions) message.NoPublishHandlerFunc {
	lm := b.logMessageFor(topic)
	handler := func(msg *message.Message) error {
		ctx := ContextWithMessageID(msg.Context(), msg.UUID)
		ctx = ContextWithMessageKey(ctx, msg.Metadata.Get(MessageKeyKey.String()))
		if lm.Subscribe {
			b.logger.DebugContext(ctx, "received message", "topic", topic,
				"message", string(msg.Payload), "x-message-id", msg.UUID)
		}

		if err := h(ctx, fromWatermill(msg)); err != nil {
			b.logger.ErrorContext(ctx, "error handling message", "error", err, "x-message-id", msg.UUID)
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
		// （handler 内已记录），返回 nil 以免触发重试中间件。
		return func(msg *message.Message) error {
			msg.Ack()
			_ = handler(msg)
			return nil
		}
	}
	return handler
}

// retryMiddleware 返回 topic 的重试中间件：事件级 Retry > Options.Retry >
// 默认 {MaxRetries: 3}；MaxRetries <= 0 表示不重试（不挂中间件）。
// 重试耗尽后消息不确认，依赖 at-least-once 语义的 Transport（如 Kafka 关闭
// 自动提交）会重投；开启自动提交时 offset 已被提交，消息可能静默丢失。
func (b *broker) retryMiddleware(topic string) (message.HandlerMiddleware, bool) {
	r := b.retryFor(topic)
	if r.MaxRetries <= 0 {
		return nil, false
	}
	retry := middleware.Retry{MaxRetries: r.MaxRetries}
	if r.Backoff > 0 {
		retry.InitialInterval = r.Backoff
		retry.MaxInterval = r.Backoff
	}
	return retry.Middleware, true
}

// retryFor 解析 topic 的重试配置：事件级 > Options.Retry > 默认 {3, 0}。
func (b *broker) retryFor(topic string) RetryOptions {
	if ev, ok := b.options.Events[topic]; ok && ev.Retry != nil {
		return *ev.Retry
	}
	if b.options.Retry != nil {
		return *b.options.Retry
	}
	return RetryOptions{MaxRetries: 3}
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
	if lm := b.logMessageFor(topic); lm.Publish {
		b.logger.DebugContext(ctx, "publishing message", "topic", topic,
			"message", string(m.Payload), "key", m.Key)
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
