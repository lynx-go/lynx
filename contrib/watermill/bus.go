// Package watermill 提供基于 watermill 的 eventbus.Bus 实现，
// 保留生产级健壮性与多后端支持，收口于一等 eventbus。
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
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
	"github.com/google/uuid"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/logging"
)

// Bus 是 watermill 驱动的 eventbus.Bus，支持多 Transport 路由与重试。
type Bus struct {
	opts   eventbus.Options
	logger *slog.Logger
	router *message.Router

	routeMu  sync.RWMutex
	routes   map[string]routeEntry
	explicit map[string]routeEntry

	mu      sync.Mutex
	pending []pendingSubscription
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
	opts        eventbus.SubscribeOptions
}

// New 创建 watermill Bus。
func New(opts eventbus.Options) eventbus.Bus {
	opts.EnsureDefaults()
	return &Bus{
		opts:     opts,
		routes:   map[string]routeEntry{},
		explicit: map[string]routeEntry{},
		logger:   slog.Default(),
	}
}

// Name 返回服务名。
func (b *Bus) Name() string { return "watermill-bus" }

// Init 初始化 router 与自动路由。
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
	router.AddPlugin(plugin.SignalsHandler)
	b.router = router

	b.routeMu.Lock()
	defer b.routeMu.Unlock()
	for _, t := range b.opts.Transports {
		for _, topic := range t.Topics() {
			if _, ok := b.explicit[topic]; ok {
				continue
			}
			if prev, ok := b.routes[topic]; ok && prev.t != t {
				return fmt.Errorf("topic %q is routed to multiple transports", topic)
			}
			b.routes[topic] = routeEntry{t: t, key: topic}
		}
	}
	return nil
}

// CheckHealth 报告是否在运行。
func (b *Bus) CheckHealth() error {
	if b.router == nil {
		return errors.New("bus is not initialized")
	}
	if b.router.IsRunning() {
		return nil
	}
	return errors.New("bus is not running")
}

// Start 启动 router。
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
			topic:       key,
			handlerName: p.handlerName,
			adapter: subscriberAdapter{
				t:    t,
				opts: eventbus.SubscribeOptions{Group: p.opts.Group, Instances: p.opts.Instances},
			},
			handler: b.wrapHandler(p.topic, p.handler, p.opts),
		})
	}
	for _, r := range registrations {
		h := b.router.AddConsumerHandler(r.handlerName, r.topic, r.adapter, r.handler)
		if retry, ok := b.retryMiddleware(r.topic); ok {
			h.AddMiddleware(retry)
		}
	}
	b.started = true
	b.pending = nil
	b.mu.Unlock()
	return b.router.Run(ctx)
}

// Stop 关闭 router。
func (b *Bus) Stop(ctx context.Context) error {
	b.mu.Lock()
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

// Publish 发布。
func (b *Bus) Publish(ctx context.Context, topic string, payload any, opts ...eventbus.PublishOption) error {
	o := &eventbus.PublishOptions{}
	for _, fn := range opts {
		fn(o)
	}
	var raw *eventbus.RawEvent
	switch v := payload.(type) {
	case *eventbus.RawEvent:
		if v == nil {
			return errors.New("eventbus: payload is typed nil *RawEvent")
		}
		raw = v
	case []byte:
		raw = &eventbus.RawEvent{ID: uuid.NewString(), Topic: topic, Payload: v, Headers: map[string]string{}, Time: time.Now()}
	case nil:
		raw = &eventbus.RawEvent{ID: uuid.NewString(), Topic: topic, Headers: map[string]string{}, Time: time.Now()}
	default:
		m := b.MarshalerFor(topic)
		if o.Marshaler != nil {
			m = o.Marshaler
		}
		bs, err := m.Marshal(payload)
		if err != nil {
			return fmt.Errorf("eventbus: marshal %q: %w", topic, err)
		}
		raw = &eventbus.RawEvent{ID: uuid.NewString(), Topic: topic, Payload: bs, Headers: map[string]string{}, Time: time.Now()}
	}
	// Clone and apply options
	raw = cloneRawEvent(raw)
	raw.Topic = topic
	if o.MessageKey != "" {
		raw.Key = o.MessageKey
	}
	if raw.Headers == nil {
		raw.Headers = map[string]string{}
	}
	maps.Copy(raw.Headers, o.Metadata)
	// Propagate attrs
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
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	raw.Topic = key // for transport, use physical
	if lm := b.logFor(topic); lm.Publish {
		b.logger.DebugContext(ctx, "publishing event", "topic", topic, "key", raw.Key)
	}
	return t.Publish(ctx, key, raw)
}

// PublishRaw 原始发布。
func (b *Bus) PublishRaw(ctx context.Context, topic string, data []byte, opts ...eventbus.PublishOption) error {
	return b.Publish(ctx, topic, data, opts...)
}

// Subscribe 订阅。
func (b *Bus) Subscribe(ctx context.Context, topic, handlerName string, h eventbus.HandlerFunc, opts ...eventbus.SubscribeOption) error {
	o := &eventbus.SubscribeOptions{}
	for _, fn := range opts {
		fn(o)
	}
	// Merge topic defaults
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
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return errors.New("cannot subscribe to a stopped bus")
	}
	if b.started {
		return errors.New("cannot subscribe to a started bus")
	}
	for _, p := range b.pending {
		if p.handlerName == handlerName {
			return fmt.Errorf("duplicate handler name %q", handlerName)
		}
	}
	b.pending = append(b.pending, pendingSubscription{topic: topic, handlerName: handlerName, handler: h, opts: *o})
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

// internal helpers

func (b *Bus) resolve(topic string) (eventbus.Transport, string, error) {
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
		// Restore attrs
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

// subscriberAdapter adapts Transport to watermill Subscriber
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
		select {
		case <-ctx.Done():
		default:
		}
	}()
	return out, nil
}
func (a subscriberAdapter) Close() error { return nil }

func toWatermill(e *eventbus.RawEvent) *message.Message {
	msg := message.NewMessage(e.ID, e.Payload)
	if e.Key != "" {
		msg.Metadata.Set("x-message-key", e.Key)
	}
	maps.Copy(msg.Metadata, e.Headers)
	return msg
}
func fromWatermill(msg *message.Message) *eventbus.RawEvent {
	e := &eventbus.RawEvent{
		ID:      msg.UUID,
		Key:     msg.Metadata.Get("x-message-key"),
		Headers: map[string]string{},
		Payload: msg.Payload,
		Time:    time.Now(),
	}
	maps.Copy(e.Headers, msg.Metadata)
	delete(e.Headers, "x-message-key")
	return e
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
