package watermill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/google/uuid"
	"github.com/lynx-go/lynx/eventbus"
)

// Bus 是 watermill 驱动的 eventbus.Bus：Router 调度 + 可插拔 Transport。
// lynx.* 强制走内置 MemoryTransport；信号只归 App（无 SignalsHandler）。
type Bus struct {
	opts     eventbus.Options
	ext      Options // watermill 扩展配置（重投上限等；eventbus.Options 已冻结）
	resolver *eventbus.Resolver
	logger   *slog.Logger
	router   *message.Router
	// warnBufferSize 记录构造时是否显式设置了 BufferSize（内存 Bus 专属）：
	// EnsureDefaults 会把零值填成 64，必须在填充前判定，Init 拿到 logger 后 Warn。
	warnBufferSize bool

	// redeliver 是 Bus 级毒消息重投计数（WK-02）：handler 终态失败按
	// handler × 消息 ID 有界累计（键含 handlerName，避免成功侧清零失败侧
	// 计数），超过上限后 Ack 丢弃，阻断 Transport 的无限重投。实现见
	// eventbus.RedeliveryLimiter（核心归属，适配器只接线）。
	redeliver *eventbus.RedeliveryLimiter

	lifecycle *MemoryTransport // 专用于 lynx.*，Bus 拥有并 Close

	routeMu  sync.RWMutex
	routes   map[string]routeEntry
	explicit map[string]routeEntry

	mu           sync.Mutex
	pending      []pendingSubscription
	handlerNames map[string]struct{}
	// subs 是订阅注册表：同一事件（逻辑 topic；消费组后端再加组）只建
	// 一条 transport 订阅，多个 handler 挂载其上进程内并行扇出；组是
	// 事件配置属性，不再是 handler 订阅参数。实现见 subscription.go。
	subs    map[subscriptionKey]*topicSubscription
	runCtx  context.Context
	started bool
	stopped bool
}

type routeEntry struct {
	t   eventbus.Transport
	key string
}

type pendingSubscription struct {
	topic       string
	handlerName string
	handler     eventbus.HandlerFunc
	resolved    eventbus.ResolvedSubscription
}

// New 创建 watermill Bus；ext 注入 watermill 特有扩展配置（可空，见 Options）。
func New(opts eventbus.Options, ext ...Option) *Bus {
	bufferSizeSet := opts.BufferSize != 0
	opts.EnsureDefaults()
	b := &Bus{
		opts:           opts,
		resolver:       eventbus.NewResolver(opts),
		routes:         map[string]routeEntry{},
		explicit:       map[string]routeEntry{},
		handlerNames:   map[string]struct{}{},
		subs:           map[subscriptionKey]*topicSubscription{},
		redeliver:      eventbus.NewRedeliveryLimiter(4096),
		logger:         slog.Default(),
		warnBufferSize: bufferSizeSet,
	}
	for _, o := range ext {
		if o != nil {
			o(&b.ext)
		}
	}
	return b
}

// Name 返回服务名。
func (b *Bus) Name() string { return "watermill-bus" }

// Route 将逻辑 topic 绑定到 Transport（Transport 侧键 = 逻辑名）。
func (b *Bus) Route(topic string, t eventbus.Transport) error {
	return b.RouteKey(topic, t, topic)
}

// RouteKey 将逻辑 topic 绑定到 Transport，并指定 Transport 侧键。
// lynx.* 只能绑到本包 MemoryTransport；否则返回错误。
func (b *Bus) RouteKey(topic string, t eventbus.Transport, key string) error {
	if t == nil {
		return errors.New("watermill: nil transport")
	}
	if eventbus.IsLifecycleTopic(topic) && !isMemoryTransport(t) {
		return fmt.Errorf("watermill: lifecycle topic %q must use MemoryTransport, got %T", topic, t)
	}
	if key == "" {
		key = topic
	}
	b.routeMu.Lock()
	defer b.routeMu.Unlock()
	e := routeEntry{t: t, key: key}
	b.explicit[topic] = e
	b.routes[topic] = e
	return nil
}

// Init 初始化 router、内置生命周期 MemoryTransport，并校验路由。
func (b *Bus) Init(ctx eventbus.InitContext) error {
	if ctx != nil {
		b.logger = ctx.Logger("service", "watermill-bus")
	}
	if b.warnBufferSize {
		b.logger.Warn("watermill: eventbus.Options.BufferSize is only honored by the memory bus; ignored")
	}
	wmLogger := b.logger
	if !b.opts.Debug {
		wmLogger = slog.New(levelFilterHandler{level: slog.LevelInfo, h: b.logger.Handler()})
	}
	logger := watermill.NewSlogLogger(wmLogger)
	router, err := message.NewRouter(message.RouterConfig{
		// 与 App StopTimeout 同量级：避免 subscriber Close 失败时卡满 30s 默认值。
		CloseTimeout: 5 * time.Second,
	}, logger)
	if err != nil {
		return err
	}
	router.AddMiddleware(middleware.Recoverer, middleware.CorrelationID)
	// 故意不装 SignalsHandler：信号只归 App。
	b.router = router

	b.lifecycle = NewMemoryTransport()

	b.routeMu.Lock()
	defer b.routeMu.Unlock()
	for _, t := range b.opts.Transports {
		for _, topic := range t.Topics() {
			if eventbus.IsLifecycleTopic(topic) && !isMemoryTransport(t) {
				return fmt.Errorf("watermill: lifecycle topic %q cannot auto-route to non-memory transport %T", topic, t)
			}
			if _, ok := b.explicit[topic]; ok {
				continue
			}
			if prev, ok := b.routes[topic]; ok && prev.t != t {
				return fmt.Errorf("topic %q is routed to multiple transports", topic)
			}
			b.routes[topic] = routeEntry{t: t, key: topic}
		}
	}
	// explicit 表只由 RouteKey 写入，其写入前已完成 lynx.* 校验——此处
	// 不再重复检查（lynx.* 的执行点收敛为：RouteKey 配置期报错 + resolve
	// 运行时强制走内置内存后端 + Init 的自动路由拒绝）。
	// DefaultTransport 可为 Kafka 等非内存后端，但不得承接 lynx.*：
	// resolve 的生命周期前缀规则优先于 DefaultTransport 回退。
	return nil
}

// CheckHealth 对齐 Router 运行态；Closed 后视为不健康。
// 注意：仅反映进程内运行标志（router 是否在跑），不做 broker 连通性检查——
// Transport 侧断连要等各自 Start/Subscribe 报错才会暴露（WK-12 语义澄清）。
func (b *Bus) CheckHealth() error {
	if b.router == nil {
		return errors.New("bus is not initialized")
	}
	b.mu.Lock()
	stopped := b.stopped
	b.mu.Unlock()
	if stopped || b.router.IsClosed() {
		return errors.New("bus is not running")
	}
	if b.router.IsRunning() {
		return nil
	}
	return errors.New("bus is not running")
}

// Start 启动空 Router（允许 0 handler），阻塞至 ctx 取消或 Close。
// Start 后 Subscribe 走 AddConsumerHandler + RunHandlers。
func (b *Bus) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errors.New("bus already stopped")
	}
	if b.started {
		b.mu.Unlock()
		return errors.New("bus already started")
	}
	if b.router == nil {
		b.mu.Unlock()
		return errors.New("bus is not initialized")
	}
	b.runCtx = ctx
	pending := append([]pendingSubscription(nil), b.pending...)
	b.pending = nil
	b.started = true
	b.mu.Unlock()

	for _, p := range pending {
		// 复审-7：pending 路径与动态路径同样复用 panic 安全包装——router
		// 内残留幽灵 handler 等场景的 panic 必须翻译为错误返回，不得击穿
		// Start 所在 goroutine（防御性补齐，与 Subscribe 对称）。
		if err := b.attachHandler(p.topic, p.handlerName, p.handler, p.resolved); err != nil {
			// WK-11：失败必须回滚 started，否则 Bus 停留在"started=true
			// 但 router 未运行"的中间态，后续动态 Subscribe 的 RunHandlers
			// 会持续报错（router 未就绪）。
			b.mu.Lock()
			b.started = false
			b.mu.Unlock()
			return err
		}
	}
	if err := b.router.Run(ctx); err != nil {
		// WK-11：router.Run 失败同样回滚。注意上游 router 的 isRunning 不
		// 会复位，此后 Bus 事实不可用——动态 Subscribe 将得到明确的
		// "router not running" 错误，而非静默假启动后无限报错。
		b.mu.Lock()
		b.started = false
		b.mu.Unlock()
		return err
	}
	return nil
}

// Stop 关闭 router 与生命周期 MemoryTransport。
// 注意（WK-10 契约）：opts.Transports / DefaultTransport 不在关闭之列——
// Transport 是独立服务（如 Kafka Transport 需 Register 交由框架托管
// Start/Stop），Bus.Stop 只负责自身与内置生命周期后端。未注册托管的
// Transport 不会随 Bus 关闭，调用方必须自行保证其生命周期。
func (b *Bus) Stop(ctx context.Context) error {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	var first error
	if b.router != nil {
		if err := b.router.Close(); err != nil {
			b.logger.ErrorContext(ctx, "error closing router", "error", err)
			first = err
		}
	}
	if b.lifecycle != nil {
		if err := b.lifecycle.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Publish 发布。
func (b *Bus) Publish(ctx context.Context, topic string, payload any, opts ...eventbus.PublishOption) error {
	o := &eventbus.PublishOptions{}
	eventbus.ApplyPublishOptions(o, opts...)
	raw, err := eventbus.BuildRawEvent(ctx, b, topic, payload, o, b.resolver.PropagateKeys())
	if err != nil {
		return err
	}
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	if lm := b.resolver.LogMessageFor(topic); lm.Publish {
		// Debug 级日志：log_message 配置实际开启的是 debug 级输出，
		// 需 --log-level=debug 才可见（WK-18 语义澄清）。
		b.logger.DebugContext(ctx, "publishing event", "topic", topic, "key", raw.Key)
	}
	return t.Publish(ctx, key, eventbus.CloneRawEvent(raw))
}

// Subscribe 订阅逻辑 topic。同一事件的多个 handler 共享一条 transport
// 订阅（进程内并行扇出）：消费组 / 实例数是事件配置属性（Topic 默认值 /
// Options.Topics），不是订阅参数。Start 后动态注册走
// AddConsumerHandler + RunHandlers。
func (b *Bus) Subscribe(ctx context.Context, topic string, h eventbus.HandlerFunc, opts ...eventbus.SubscribeOption) error {
	o := &eventbus.SubscribeOptions{}
	eventbus.ApplySubscribeOptions(o, opts...)
	// 有效订阅一次解析（Topic 默认值 + 调用级 + Options.Topics/全局），
	// 投递路径直接消费结果。
	resolved := b.resolver.ResolveSubscription(topic, *o)
	handlerName := resolved.HandlerName
	if h == nil {
		return errors.New("handler is nil")
	}

	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errors.New("cannot subscribe to a stopped bus")
	}
	if _, dup := b.handlerNames[handlerName]; dup {
		b.mu.Unlock()
		return fmt.Errorf("duplicate handler name %q", handlerName)
	}
	for _, p := range b.pending {
		if p.handlerName == handlerName {
			b.mu.Unlock()
			return fmt.Errorf("duplicate handler name %q", handlerName)
		}
	}
	started := b.started
	if !started {
		b.handlerNames[handlerName] = struct{}{}
		b.pending = append(b.pending, pendingSubscription{topic: topic, handlerName: handlerName, handler: h, resolved: resolved})
		b.mu.Unlock()
		return nil
	}
	b.handlerNames[handlerName] = struct{}{}
	runCtx := b.runCtx
	b.mu.Unlock()

	if err := b.attachHandler(topic, handlerName, h, resolved); err != nil {
		if !errors.Is(err, errHandlerNameTaken) {
			// handler 未进入订阅（resolve 失败、panic 翻译等），回滚名字
			// 是安全的；errHandlerNameTaken 例外：router 内已有同名幽灵
			// 订阅，名字事实已被占用，回滚只会让下次重试撞上 panic 路径。
			b.mu.Lock()
			delete(b.handlerNames, handlerName)
			b.mu.Unlock()
		}
		return err
	}
	if runCtx == nil {
		runCtx = ctx
	}
	if err := b.router.RunHandlers(runCtx); err != nil {
		// WK-03：RunHandlers 失败不回滚 handlerNames——handler 已登记进
		// router（router 尚在启动时会随后续 Run 生效；router 启动失败则
		// 一并失效），回滚会造成"幽灵订阅"：router 内残留同名 handler，
		// 用户重试同名 Subscribe 必然触发 watermill 的
		// DuplicateHandlerNameError panic。保持占用并以错误明示。
		return fmt.Errorf("watermill: handler %q registered but not started (router not running): %w; "+
			"do not resubscribe under a different handler name: the handler stays registered and will "+
			"take effect once the router starts; if the bus start failed permanently, the handler name "+
			"stays taken — check the bus Start error instead", handlerName, err)
	}
	return nil
}

// errHandlerNameTaken 标记"router 内已存在同名 handler"（panic 翻译而来）：
// 此名已被占用，Subscribe 失败但不得回滚 handlerNames（WK-03）。
var errHandlerNameTaken = errors.New("handler name already exists in router")

// MarshalerFor 返回序列化器（委托共享 Resolver，查找序见其文档）。
func (b *Bus) MarshalerFor(topic string) eventbus.Marshaler {
	return b.resolver.MarshalerFor(topic)
}

func (b *Bus) resolve(topic string) (eventbus.Transport, string, error) {
	// 方案 B：lynx.* 前缀优先于 DefaultTransport / 用户路由表
	if eventbus.IsLifecycleTopic(topic) {
		if b.lifecycle == nil {
			return nil, "", errors.New("watermill: lifecycle transport not initialized")
		}
		return b.lifecycle, topic, nil
	}
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
	if b.opts.DefaultTransport != nil {
		return b.opts.DefaultTransport, topic, nil
	}
	return nil, "", fmt.Errorf("no transport for topic %q", topic)
}

func isMemoryTransport(t eventbus.Transport) bool {
	_, ok := t.(*MemoryTransport)
	return ok
}

// subscriberAdapter 把 eventbus.Transport 接到 Watermill Subscriber。
// Close 必须取消 Subscribe 派生的 ctx 并等待转发 goroutine 退出：
// Watermill handleClose 先调 Subscriber.Close，成功后才 cancel handler ctx；
// 空 Close 会使 Transport 订阅链永不拆掉，router.Close 卡满 CloseTimeout。
type subscriberAdapter struct {
	t    eventbus.Transport
	opts eventbus.SubscribeOptions
	// forwardAck 注入 Bus 的确认转达函数（携带订阅 ctx 与 logger，WK-14）。
	forwardAck func(ctx context.Context, msg *message.Message, d eventbus.Delivery, release func())
	// sem 是订阅级在途上限（nil = 不限制）：在把消息交给 Router 之前占用，
	// Ack/Nack/订阅关停时释放。限流点放在这里而不是 dispatcher——只限执行
	// 挡不住 Router 的每消息 goroutine 堆积（阻塞的 goroutine 仍持有消息）；
	// 在这里阻塞才能让 Router 停读、transport 投递链停读（真背压）。
	sem chan struct{}

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *subscriberAdapter) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	subCtx, cancel := context.WithCancel(ctx)
	ch, err := a.t.Subscribe(subCtx, topic, a.opts)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan *message.Message)
	done := make(chan struct{})
	a.mu.Lock()
	a.cancel = cancel
	a.done = done
	a.mu.Unlock()
	go func() {
		defer close(done)
		defer close(out)
		defer cancel()
		for {
			select {
			case <-subCtx.Done():
				return
			case d, ok := <-ch:
				if !ok {
					return
				}
				// 订阅级在途上限：占用不到槽位就不把消息交给 Router（消息
				// 留在 transport 侧，形成背压）；订阅关停时交还 Transport。
				var release func()
				if a.sem != nil {
					select {
					case a.sem <- struct{}{}:
					case <-subCtx.Done():
						d.NackOnce()
						return
					}
					release = sync.OnceFunc(func() { <-a.sem })
				}
				msg := ToMessage(d.Event)
				// Router 对副本的 Ack/Nack 转达到 Transport Delivery（Kafka offset / gochannel）。
				a.forwardAck(subCtx, msg, d, release)
				select {
				case out <- msg:
				case <-subCtx.Done():
					msg.Nack()
					return
				}
			}
		}
	}()
	return out, nil
}

func (a *subscriberAdapter) Close() error {
	a.mu.Lock()
	cancel := a.cancel
	done := a.done
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

// forwardDeliveryAck 在 Router 确认/拒绝副本消息时，调用 Transport 侧
// Ack/Nack，并释放订阅级在途槽位（release；nil 表示不限并发）。不设人为
// 超时（复审-5）：30s 固定上限会对合法慢 handler（> 30s 才返回）截断确认
// 转达，AutoCommit=false 下 offset 永不提交、消息重复消费——正常运行时等待
// 时长应完全由 handler 决定。退出分支只有订阅 ctx 取消（Bus/订阅关停）：
// 放弃等待并 Warn，未确认的后果由 Transport 的重投/超时语义兜底。
// 已知取舍（WK-14 原始 Low 项保留）：handler 挂死且订阅永不关停时该
// goroutine 常驻（每条 in-flight 消息一个，受 Concurrency 上限约束）；
// 关停路径（adapter.Close → cancel）总能释放，接受此泄漏换取慢 handler 的
// 正确性。
func (b *Bus) forwardDeliveryAck(ctx context.Context, msg *message.Message, d eventbus.Delivery, release func()) {
	logger := b.logger
	go func() {
		defer func() {
			if release != nil {
				release()
			}
		}()
		select {
		case <-msg.Acked():
			d.AckOnce()
		case <-msg.Nacked():
			d.NackOnce()
		case <-ctx.Done():
			logger.Warn("subscription closed before message was confirmed; transport-side confirm dropped",
				"message_id", msg.UUID, "cause", ctx.Err())
		}
	}()
}

// ToMessage 将 RawEvent 转为 watermill 消息（wire 元数据经 EncodeWireMetadata；
// ID 为空时生成）。设计文档 §5.1 单一映射点：watermill 生态的 Transport 实现
// （如 contrib/watermill-kafka）必须复用本函数，禁止各写一份。
func ToMessage(e *eventbus.RawEvent) *message.Message {
	if e == nil {
		return message.NewMessage("", nil)
	}
	id := e.ID
	if id == "" {
		id = uuid.NewString()
	}
	msg := message.NewMessage(id, e.Payload)
	for k, v := range eventbus.EncodeWireMetadata(e) {
		msg.Metadata.Set(k, v)
	}
	return msg
}

// FromMessage 将 watermill 消息还原为 RawEvent（DecodeWireMetadata）。
// 与 ToMessage 对称，同为 §5.1 单一映射点。
func FromMessage(msg *message.Message) *eventbus.RawEvent {
	if msg == nil {
		return &eventbus.RawEvent{Headers: map[string]string{}}
	}
	meta := map[string]string{}
	maps.Copy(meta, msg.Metadata)
	return eventbus.DecodeWireMetadata(msg.UUID, msg.Payload, meta)
}

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

var _ eventbus.Bus = (*Bus)(nil)
