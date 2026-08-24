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
	"github.com/lynx-go/lynx/logging"
)

// Bus 是 watermill 驱动的 eventbus.Bus：Router 调度 + 可插拔 Transport。
// lynx.* 强制走内置 MemoryTransport；信号只归 App（无 SignalsHandler）。
type Bus struct {
	opts   eventbus.Options
	logger *slog.Logger
	router *message.Router

	lifecycle *MemoryTransport // 专用于 lynx.*，Bus 拥有并 Close

	routeMu  sync.RWMutex
	routes   map[string]routeEntry
	explicit map[string]routeEntry

	mu           sync.Mutex
	pending      []pendingSubscription
	handlerNames map[string]struct{}
	runCtx       context.Context
	started      bool
	stopped      bool
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

// New 创建 watermill Bus。
func New(opts eventbus.Options) *Bus {
	opts.EnsureDefaults()
	return &Bus{
		opts:         opts,
		routes:       map[string]routeEntry{},
		explicit:     map[string]routeEntry{},
		handlerNames: map[string]struct{}{},
		logger:       slog.Default(),
	}
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
	router, err := message.NewRouter(message.RouterConfig{}, logger)
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
	if b.opts.DefaultTransport != nil && !isMemoryTransport(b.opts.DefaultTransport) {
		// DefaultTransport 可为 Kafka；不得承接 lynx.*（resolve 前缀规则优先）。
	}
	return nil
}

// CheckHealth 对齐 Router 运行态；Closed 后视为不健康。
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
		if err := b.addHandler(p.topic, p.handlerName, p.handler, p.opts); err != nil {
			return err
		}
	}
	return b.router.Run(ctx)
}

// Stop 关闭 router 与生命周期 MemoryTransport。
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
		b.logger.DebugContext(ctx, "publishing event", "topic", topic, "key", raw.Key)
	}
	return t.Publish(ctx, key, cloneRawEvent(raw))
}

// PublishRaw 原始发布。
func (b *Bus) PublishRaw(ctx context.Context, topic string, data []byte, opts ...eventbus.PublishOption) error {
	return b.Publish(ctx, topic, data, opts...)
}

// Subscribe 订阅；Start 后动态注册（AddConsumerHandler + RunHandlers）。
func (b *Bus) Subscribe(ctx context.Context, topic, handlerName string, h eventbus.HandlerFunc, opts ...eventbus.SubscribeOption) error {
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
	if handlerName == "" {
		return errors.New("handler name is required")
	}
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
		b.pending = append(b.pending, pendingSubscription{topic: topic, handlerName: handlerName, handler: h, opts: *o})
		b.mu.Unlock()
		return nil
	}
	b.handlerNames[handlerName] = struct{}{}
	runCtx := b.runCtx
	b.mu.Unlock()

	if err := b.addHandler(topic, handlerName, h, *o); err != nil {
		b.mu.Lock()
		delete(b.handlerNames, handlerName)
		b.mu.Unlock()
		return err
	}
	if runCtx == nil {
		runCtx = ctx
	}
	return b.router.RunHandlers(runCtx)
}

func (b *Bus) addHandler(topic, handlerName string, h eventbus.HandlerFunc, opts eventbus.SubscribeOptions) error {
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	adapter := subscriberAdapter{
		t:    t,
		opts: eventbus.SubscribeOptions{Group: opts.Group, Instances: opts.Instances},
	}
	handler := b.wrapHandler(topic, h, opts)
	hh := b.router.AddConsumerHandler(handlerName, key, adapter, handler)
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
		return func(msg *message.Message) error {
			msg.Ack()
			_ = handler(msg)
			return nil
		}
	}
	return handler
}

type subscriberAdapter struct {
	t    eventbus.Transport
	opts eventbus.SubscribeOptions
}

func (a subscriberAdapter) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	ch, err := a.t.Subscribe(ctx, topic, a.opts)
	if err != nil {
		return nil, err
	}
	out := make(chan *message.Message)
	go func() {
		defer close(out)
		for raw := range ch {
			out <- toWatermill(raw)
		}
	}()
	return out, nil
}
func (a subscriberAdapter) Close() error { return nil }

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
func (f levelFilterHandler) Handle(ctx context.Context, r slog.Record) error { return f.h.Handle(ctx, r) }
func (f levelFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelFilterHandler{f.level, f.h.WithAttrs(attrs)}
}
func (f levelFilterHandler) WithGroup(name string) slog.Handler {
	return levelFilterHandler{f.level, f.h.WithGroup(name)}
}

var _ eventbus.Bus = (*Bus)(nil)
