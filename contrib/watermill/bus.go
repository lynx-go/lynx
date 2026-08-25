package watermill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/google/uuid"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/logging"
)

// Bus 是 watermill 驱动的 eventbus.Bus：Router 调度 + 可插拔 Transport。
// lynx.* 强制走内置 MemoryTransport；信号只归 App（无 SignalsHandler）。
type Bus struct {
	opts   eventbus.Options
	ext    Options // watermill 扩展配置（重投上限等；eventbus.Options 已冻结）
	logger *slog.Logger
	router *message.Router

	// redeliver 是 Bus 级毒消息重投计数（WK-02）：handler 终态失败按
	// handler × 消息 ID 有界累计（复审-4：键含 handlerName，避免成功侧
	// 清零失败侧计数），超过上限后 Ack 丢弃，阻断 Transport 的无限重投。
	redeliver *redeliveryLimiter

	lifecycle *MemoryTransport // 专用于 lynx.*，Bus 拥有并 Close

	routeMu  sync.RWMutex
	routes   map[string]routeEntry
	explicit map[string]routeEntry

	mu           sync.Mutex
	pending      []pendingSubscription
	handlerNames map[string]struct{}
	// groupClaims 记录非内存 Transport 上（transport × 订阅键 × 消费组）
	// 的 handler 占用（WK-01）：Kafka 等消费组后端上两个 handler 共用
	// 同一 topic+组会静默瓜分分区。内存 Transport 是广播语义，不登记。
	groupClaims map[claimKey]string
	runCtx      context.Context
	started     bool
	stopped     bool
}

// claimKey 是 groupClaims 的键。group 为空串表示"Transport 配置的默认组"
// （如 kafka consumer.group_id）：同键下第二个 handler 仍会落到同一组，
// 同样必须拒绝。t 必须可比较（仓库内 Transport 实现均为指针型）。
type claimKey struct {
	t     eventbus.Transport
	key   string
	group string
}

type routeEntry struct {
	t   eventbus.Transport
	key string
}

type pendingSubscription struct {
	topic       string
	handlerName string
	handler     eventbus.HandlerFunc
	opts        eventbus.SubscribeOptions
}

// New 创建 watermill Bus；ext 注入 watermill 特有扩展配置（可空，见 Options）。
func New(opts eventbus.Options, ext ...Option) *Bus {
	opts.EnsureDefaults()
	b := &Bus{
		opts:         opts,
		routes:       map[string]routeEntry{},
		explicit:     map[string]routeEntry{},
		handlerNames: map[string]struct{}{},
		groupClaims:  map[claimKey]string{},
		redeliver:    newRedeliveryLimiter(4096),
		logger:       slog.Default(),
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
	for topic, e := range b.explicit {
		if eventbus.IsLifecycleTopic(topic) && !isMemoryTransport(e.t) {
			return fmt.Errorf("watermill: lifecycle topic %q must use MemoryTransport, got %T", topic, e.t)
		}
	}
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
		if err := b.addHandlerSafe(p.topic, p.handlerName, p.handler, p.opts); err != nil {
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
	var raw *eventbus.RawEvent
	var eventTime time.Time
	switch v := payload.(type) {
	case *eventbus.RawEvent:
		if v == nil {
			return errors.New("eventbus: payload is typed nil *RawEvent")
		}
		raw = cloneRawEvent(v)
		eventTime = v.Time
	case []byte:
		raw = &eventbus.RawEvent{ID: uuid.NewString(), Payload: v, Headers: map[string]string{}}
	case nil:
		raw = &eventbus.RawEvent{ID: uuid.NewString(), Headers: map[string]string{}}
	default:
		m := eventbus.ResolvePublishMarshaler(b, topic, nil, o.Marshaler)
		bs, err := m.Marshal(payload)
		if err != nil {
			return fmt.Errorf("eventbus: marshal %q: %w", topic, err)
		}
		raw = &eventbus.RawEvent{ID: uuid.NewString(), Payload: bs, Headers: map[string]string{}}
	}
	raw.Topic = topic
	if o.MessageKey != "" {
		raw.Key = o.MessageKey
	}
	if raw.Headers == nil {
		raw.Headers = map[string]string{}
	}
	maps.Copy(raw.Headers, o.Metadata)
	for k := range raw.Headers {
		if k == eventbus.MetaMessageKey || k == eventbus.MetaEventTime || k == eventbus.MetaLogicalTopic {
			delete(raw.Headers, k)
		}
	}
	for _, k := range b.propagateKeys() {
		if _, ok := raw.Headers[k]; ok {
			continue
		}
		for _, a := range logging.AttrsFrom(ctx) {
			if a.Key == k {
				raw.Headers[k] = a.Value.String()
				break
			}
		}
	}
	if eventTime.IsZero() {
		eventTime = time.Now()
	}
	raw.Time = eventTime
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	if lm := b.logFor(topic); lm.Publish {
		// Debug 级日志：log_message 配置实际开启的是 debug 级输出，
		// 需 --log-level=debug 才可见（WK-18 语义澄清）。
		b.logger.DebugContext(ctx, "publishing event", "topic", topic, "key", raw.Key)
	}
	return t.Publish(ctx, key, cloneRawEvent(raw))
}

// PublishRaw 原始发布。
func (b *Bus) PublishRaw(ctx context.Context, topic string, data []byte, opts ...eventbus.PublishOption) error {
	return b.Publish(ctx, topic, data, opts...)
}

// Subscribe 订阅；Start 后动态注册（AddConsumerHandler + RunHandlers）。
func (b *Bus) Subscribe(ctx context.Context, topic string, h eventbus.HandlerFunc, opts ...eventbus.SubscribeOption) error {
	o := &eventbus.SubscribeOptions{}
	eventbus.ApplySubscribeOptions(o, opts...)
	if cfg, ok := b.opts.Topics[topic]; ok {
		if o.Group == "" {
			o.Group = cfg.Group
		}
		if o.Instances == 0 {
			o.Instances = cfg.Instances
		}
		if !o.AutoAck && cfg.AutoAck {
			o.AutoAck = true
		}
		if !o.ContinueOnError && cfg.ContinueOnError {
			o.ContinueOnError = true
		}
	}
	handlerName := o.HandlerName
	if handlerName == "" {
		handlerName = topic
	}
	if h == nil {
		return errors.New("handler is nil")
	}
	// WK-01：先解析目标 Transport，供消费组占用检查。Kafka 等消费组后端上
	// 两个不同 handler 共用同一 topic+组会静默瓜分分区（各收一半消息），
	// 必须在订阅期显式拒绝；内存 Transport 是广播语义，不受此限制。
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
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
	if err := b.claimGroup(t, topic, key, o.Group, handlerName); err != nil {
		b.mu.Unlock()
		return err
	}
	started := b.started
	if !started {
		b.handlerNames[handlerName] = struct{}{}
		b.pending = append(b.pending, pendingSubscription{topic: topic, handlerName: handlerName, handler: h, opts: *o})
		b.mu.Unlock()
		return nil
	}
	b.handlerNames[handlerName] = struct{}{}
	runCtx := b.runCtx
	b.mu.Unlock()

	if err := b.addHandlerSafe(topic, handlerName, h, *o); err != nil {
		if !errors.Is(err, errHandlerNameTaken) {
			// handler 未进入 router（resolve 失败、panic 翻译等），回滚占用
			// 是安全的；errHandlerNameTaken 例外：router 内已有同名幽灵
			// handler，名字事实已被占用，回滚只会让下次重试撞上 panic 路径。
			b.mu.Lock()
			delete(b.handlerNames, handlerName)
			// 复审-1：handler 未进入 router 时消费组占用同样必须回滚——
			// 只回滚 handlerNames 会留下 groupClaims 残留，一次订阅失败即
			// 永久锁死该 topic+组（同组不同名的后续订阅全部被拒绝）。
			b.releaseGroupClaim(t, key, o.Group)
			b.mu.Unlock()
		}
		return err
	}
	if runCtx == nil {
		runCtx = ctx
	}
	if err := b.router.RunHandlers(runCtx); err != nil {
		// WK-03：RunHandlers 失败不回滚 handlerNames/groupClaims——handler
		// 已登记进 router（router 尚在启动时会随后续 Run 生效；router 启动
		// 失败则一并失效），回滚会造成"幽灵订阅"：router 内残留同名
		// handler，用户重试同名 Subscribe 必然触发 watermill 的
		// DuplicateHandlerNameError panic。保持占用并以错误明示。
		// 复审-2：文案必须如实描述两种走向且警告勿换名重订阅——该订阅
		// 已登记，Bus/Router 最终启动成功时它将生效，此时换名再订一份
		// 会导致同一 topic+组双消费（或直接撞上 WK-01 消费组占用检查）。
		return fmt.Errorf("watermill: subscription %q registered but not started (router not running): %w; "+
			"do not resubscribe under a different handler name: the subscription stays registered and will "+
			"take effect once the router starts (a second subscription to the same topic and group would "+
			"double-consume); if the bus start failed permanently, the handler name stays taken — "+
			"check the bus Start error instead", handlerName, err)
	}
	return nil
}

// errHandlerNameTaken 标记"router 内已存在同名 handler"（panic 翻译而来）：
// 此名已被占用，Subscribe 失败但不得回滚 handlerNames（WK-03）。
var errHandlerNameTaken = errors.New("handler name already exists in router")

// claimGroup 登记非内存 Transport 上（订阅键 × 消费组）的 handler 占用，
// 已被其他 handlerName 占用时返回明确错误（WK-01）。Bus 无 Unsubscribe
// API、进程内订阅一般不撤销，因此占用后不释放（宁可误拒，不可静默瓜分）。
// 已知局限一：某 handler 显式指定的 group 恰好等于另一 handler 留空的
// Transport 默认组（如 kafka consumer.group_id）时，Bus 看不到默认组名，
// 无法识别冲突——同 topic 的多个 handler 要么全部显式配置组，要么保持
// 单 handler + instances。
// 已知局限二（复审-9，claim 粒度）：两个不同逻辑 topic 路由到同一
// Transport、各自配置的物理 topics 重叠、又共用同一（显式或默认）消费组
// 时，claim 键（transport × 订阅键 × 组）互不相同，本检查不拦截——
// Kafka 侧它们仍会并入同一消费组瓜分分区。物理 topics 存在重叠的部署
// 必须为各逻辑 topic 显式配置互不相同的消费组。
func (b *Bus) claimGroup(t eventbus.Transport, topic, key, group, handlerName string) error {
	if t == nil || isMemoryTransport(t) {
		return nil
	}
	// 不可比较的值类型 Transport 无法作为 map key 跟踪（仓库内实现均为
	// 指针型），放弃占用检查而非在 map 写入时 panic。
	if !reflect.TypeOf(t).Comparable() {
		return nil
	}
	k := claimKey{t: t, key: key, group: group}
	if prev, ok := b.groupClaims[k]; ok && prev != handlerName {
		groupLabel := group
		if groupLabel == "" {
			groupLabel = "(transport default group)"
		}
		return fmt.Errorf(
			"watermill: topic %q (transport key %q, group %s) is already consumed by handler %q on %T; "+
				"two handlers sharing one consumer group silently split partitions and each receives only part of the messages. "+
				"Use a distinct group per handler (WithGroup or bus topic group) for broadcast semantics, "+
				"or keep a single handler with WithInstances for competing consumers",
			topic, key, groupLabel, prev, t)
	}
	b.groupClaims[k] = handlerName
	return nil
}

// releaseGroupClaim 回滚 claimGroup 的登记（复审-1）：动态订阅在登记后、
// handler 进入 router 前失败时调用，避免残留 claim 永久锁死该 topic+组。
// 与 claimGroup 的跳过条件保持一致——不可比较的 Transport 键连 delete 都
// 会 panic，绝不能直接 delete。调用方必须已持有 b.mu。
func (b *Bus) releaseGroupClaim(t eventbus.Transport, key, group string) {
	if t == nil || isMemoryTransport(t) {
		return
	}
	if !reflect.TypeOf(t).Comparable() {
		return
	}
	delete(b.groupClaims, claimKey{t: t, key: key, group: group})
}

// addHandlerSafe 是 addHandler 的 panic 安全包装（WK-03）：watermill router
// 对重名 handler 直接 panic（DuplicateHandlerNameError），正常路径已由
// handlerNames 查重拦截，但历史缺陷或外部操作残留的"幽灵 handler"会绕过
// 查重——动态订阅不得击穿进程，此处把 panic 翻译为错误返回。
// 非 duplicate panic 附 debug.Stack()（复审-8）：仅 %v 的 panic 值无堆栈，
// 未知 panic 源无从排查。
func (b *Bus) addHandlerSafe(topic, handlerName string, h eventbus.HandlerFunc, opts eventbus.SubscribeOptions) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if dup, ok := r.(message.DuplicateHandlerNameError); ok {
				err = fmt.Errorf("%w %q: %s", errHandlerNameTaken, dup.HandlerName, dup.Error())
				return
			}
			err = fmt.Errorf("watermill: add handler %q panicked: %v\n%s", handlerName, r, debug.Stack())
		}
	}()
	return b.addHandler(topic, handlerName, h, opts)
}

func (b *Bus) addHandler(topic, handlerName string, h eventbus.HandlerFunc, opts eventbus.SubscribeOptions) error {
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	adapter := &subscriberAdapter{
		t:          t,
		opts:       eventbus.SubscribeOptions{Group: opts.Group, Instances: opts.Instances},
		forwardAck: b.forwardDeliveryAck,
	}
	handler := b.wrapHandler(topic, h, opts)
	hh := b.router.AddConsumerHandler(handlerName, key, adapter, handler)
	// WK-02：重投上限中间件必须先于 Retry 添加——先添加者位于调用栈最
	// 外层，这样它按"投递轮次"计数（Retry 耗尽后的终态失败每轮只计一次），
	// Retry 的内层多次重试不会被重复计数。
	if limit, ok := b.maxRedeliveriesFor(topic); ok {
		hh.AddMiddleware(b.redeliveryMiddleware(handlerName, topic, limit))
	}
	if retry, ok := b.retryMiddleware(topic); ok {
		hh.AddMiddleware(retry)
	}
	return nil
}

// MarshalerFor 返回序列化器。
func (b *Bus) MarshalerFor(topic string) eventbus.Marshaler {
	if m, ok := b.opts.TopicMarshalers[topic]; ok {
		return m
	}
	if b.opts.Marshaler != nil {
		return b.opts.Marshaler
	}
	return eventbus.JSONMarshaler{}
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

func (b *Bus) propagateKeys() []string {
	if b.opts.PropagateAttrs != nil {
		return b.opts.PropagateAttrs
	}
	return []string{logging.FieldRequestID, logging.FieldUserID}
}

func (b *Bus) logFor(topic string) eventbus.LogMessageOptions {
	if cfg, ok := b.opts.Topics[topic]; ok && cfg.LogMessage != nil {
		return *cfg.LogMessage
	}
	if b.opts.LogMessage != nil {
		return *b.opts.LogMessage
	}
	return eventbus.LogMessageOptions{}
}

func (b *Bus) retryFor(topic string) eventbus.RetryOptions {
	if cfg, ok := b.opts.Topics[topic]; ok && cfg.Retry != nil {
		return *cfg.Retry
	}
	if b.opts.Retry != nil {
		return *b.opts.Retry
	}
	return eventbus.RetryOptions{MaxRetries: 3}
}

func (b *Bus) retryMiddleware(topic string) (message.HandlerMiddleware, bool) {
	r := b.retryFor(topic)
	if r.MaxRetries <= 0 {
		return nil, false
	}
	retry := middleware.Retry{MaxRetries: r.MaxRetries}
	if r.Backoff > 0 {
		// 行为冻结（WK-17）：InitialInterval=MaxInterval=Backoff 会把
		// middleware.Retry 的指数退避压成固定间隔——每次重试都等 Backoff，
		// 与字段名 "Backoff" 暗示的指数增长不符。保持现语义不改（避免
		// 静默拉长既有用户的重试时延），需要指数退避时调小 Backoff 补偿。
		retry.InitialInterval = r.Backoff
		retry.MaxInterval = r.Backoff
	}
	return retry.Middleware, true
}

func (b *Bus) wrapHandler(topic string, h eventbus.HandlerFunc, opts eventbus.SubscribeOptions) message.NoPublishHandlerFunc {
	lm := b.logFor(topic)
	handler := func(msg *message.Message) error {
		raw := fromWatermill(msg)
		raw.Topic = topic
		ctx := msg.Context()
		existing := map[string]struct{}{}
		for _, a := range logging.AttrsFrom(ctx) {
			existing[a.Key] = struct{}{}
		}
		var attrs []slog.Attr
		for _, k := range b.propagateKeys() {
			if _, ok := existing[k]; ok {
				continue
			}
			if v, ok := raw.Headers[k]; ok && v != "" {
				attrs = append(attrs, slog.String(k, v))
			}
		}
		ctx = logging.WithAttrs(ctx, attrs...)
		if lm.Subscribe {
			// Debug 级日志：log_message 配置实际开启的是 debug 级输出，
			// 需 --log-level=debug 才可见（WK-18 语义澄清）。
			b.logger.DebugContext(ctx, "received event", "topic", topic)
		}
		if err := h(ctx, raw); err != nil {
			b.logger.ErrorContext(ctx, "handler failed", "error", err)
			if opts.ContinueOnError {
				msg.Ack()
				return nil
			}
			return err
		}
		msg.Ack()
		return nil
	}
	if opts.AutoAck {
		// fire-and-forget（WK-13 语义澄清）：先 Ack 后执行 handler，handler
		// 错误仅记日志、消息不会重投——外层 Retry 中间件看到的一直是成功，
		// 因此 AutoAck 与 Retry 组合时重试不会发生，终态失败计数（重投
		// 上限）也不会累积。仅用于可容忍丢失的旁路事件。
		return func(msg *message.Message) error {
			msg.Ack()
			_ = handler(msg)
			return nil
		}
	}
	return handler
}

// subscriberAdapter 把 eventbus.Transport 接到 Watermill Subscriber。
// Close 必须取消 Subscribe 派生的 ctx 并等待转发 goroutine 退出：
// Watermill handleClose 先调 Subscriber.Close，成功后才 cancel handler ctx；
// 空 Close 会使 Transport 订阅链永不拆掉，router.Close 卡满 CloseTimeout。
type subscriberAdapter struct {
	t    eventbus.Transport
	opts eventbus.SubscribeOptions
	// forwardAck 注入 Bus 的确认转达函数（携带订阅 ctx 与 logger，WK-14）。
	forwardAck func(ctx context.Context, msg *message.Message, d eventbus.Delivery)

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
				msg := toWatermill(d.Event)
				// Router 对副本的 Ack/Nack 转达到 Transport Delivery（Kafka offset / gochannel）。
				a.forwardAck(subCtx, msg, d)
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
// Ack/Nack。不设人为超时（复审-5）：30s 固定上限会对合法慢 handler（>
// 30s 才返回）截断确认转达，AutoCommit=false 下 offset 永不提交、消息
// 重复消费——正常运行时等待时长应完全由 handler 决定。退出分支只有
// 订阅 ctx 取消（Bus/订阅关停）：放弃等待并 Warn，未确认的后果由
// Transport 的重投/超时语义兜底。
// 已知取舍（WK-14 原始 Low 项保留）：handler 挂死且订阅永不关停时该
// goroutine 常驻（每条 in-flight 消息一个）；关停路径（adapter.Close →
// cancel）总能释放，接受此泄漏换取慢 handler 的正确性。
func (b *Bus) forwardDeliveryAck(ctx context.Context, msg *message.Message, d eventbus.Delivery) {
	logger := b.logger
	go func() {
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

func toWatermill(e *eventbus.RawEvent) *message.Message {
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

func fromWatermill(msg *message.Message) *eventbus.RawEvent {
	if msg == nil {
		return &eventbus.RawEvent{Headers: map[string]string{}}
	}
	meta := map[string]string{}
	maps.Copy(meta, msg.Metadata)
	return eventbus.DecodeWireMetadata(msg.UUID, msg.Payload, meta)
}

func cloneRawEvent(e *eventbus.RawEvent) *eventbus.RawEvent {
	cp := *e
	if e.Headers != nil {
		cp.Headers = make(map[string]string, len(e.Headers))
		maps.Copy(cp.Headers, e.Headers)
	}
	if e.Payload != nil {
		cp.Payload = append([]byte(nil), e.Payload...)
	}
	return &cp
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
