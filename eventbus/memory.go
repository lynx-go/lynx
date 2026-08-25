package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx/logging"
)

// memoryBus 是默认的进程内 Bus：零依赖、开箱即用、支持 Start 后动态订阅。
//
// 投递语义（与持久化 Bus 的关键差异，选型时必读）：
// 内存 Bus 是 at-most-once——订阅者缓冲满时非阻塞丢弃（仅 Error 日志，
// 见 dispatch）；handler 重试耗尽同样丢弃（见 handleWithRetry）。
// 原因：状态协同场景不应反压发布者，更不该阻塞进程内事件循环。
// 持久化 Bus（如 Kafka，见 contrib/watermill-kafka）语义相反：
// at-least-once，处理失败无限重投。跨 Bus 迁移前先确认业务能接受
// 哪一侧的丢失/重复语义，必要时内存侧调大 BufferSize 或业务侧幂等。
type memoryBus struct {
	opts   Options
	logger *slog.Logger

	mu          sync.RWMutex
	subs        map[string][]*subscriber // topic -> subs
	handlerName map[string]struct{}
	running     atomic.Bool
	closed      atomic.Bool
}

type subscriber struct {
	handlerName string
	topic       string
	ch          chan *RawEvent
	handler     HandlerFunc
	opts        SubscribeOptions
	cancel      context.CancelFunc
}

// NewMemoryBus 创建内存 Bus，opts 为空时使用默认值。
func NewMemoryBus(opts Options) Bus {
	opts.EnsureDefaults()
	return &memoryBus{
		opts:        opts,
		subs:        map[string][]*subscriber{},
		handlerName: map[string]struct{}{},
		logger:      slog.Default(),
	}
}

// Option 配置 Bus Options。
type Option func(*Options)

// WithBufferSize 设置每订阅者通道缓冲。
func WithBufferSize(n int) Option { return func(o *Options) { o.BufferSize = n } }

// WithMarshaler 设置全局序列化器。
func WithMarshaler(m Marshaler) Option { return func(o *Options) { o.Marshaler = m } }

// WithRetry 设置全局重试。
func WithRetry(r RetryOptions) Option { return func(o *Options) { o.Retry = &r } }

// Name 返回服务名。
func (b *memoryBus) Name() string { return "bus-memory" }

// Init 捕获日志实例。
func (b *memoryBus) Init(ctx InitContext) error {
	if ctx != nil {
		b.logger = ctx.Logger("service", "bus")
	}
	return nil
}

// Start 标记运行并阻塞至 ctx 取消。
func (b *memoryBus) Start(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("bus already closed")
	}
	b.running.Store(true)
	<-ctx.Done()
	b.running.Store(false)
	return nil
}

// Stop 关闭全部订阅通道。
func (b *memoryBus) Stop(ctx context.Context) error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, list := range b.subs {
		for _, sub := range list {
			sub.cancel()
			close(sub.ch)
		}
	}
	b.subs = map[string][]*subscriber{}
	b.handlerName = map[string]struct{}{}
	b.running.Store(false)
	return nil
}

// CheckHealth 报告是否在运行。
func (b *memoryBus) CheckHealth() error {
	if !b.running.Load() {
		return errors.New("bus is not running")
	}
	return nil
}

// MarshalerFor 返回主题序列化器。
func (b *memoryBus) MarshalerFor(topic string) Marshaler {
	if m, ok := b.opts.TopicMarshalers[topic]; ok {
		return m
	}
	if b.opts.Marshaler != nil {
		return b.opts.Marshaler
	}
	return JSONMarshaler{}
}

func (b *memoryBus) propagateKeys() []string {
	if b.opts.PropagateAttrs != nil {
		return b.opts.PropagateAttrs
	}
	return []string{logging.FieldRequestID, logging.FieldUserID}
}

func (b *memoryBus) logFor(topic string) LogMessageOptions {
	if cfg, ok := b.opts.Topics[topic]; ok && cfg.LogMessage != nil {
		return *cfg.LogMessage
	}
	if b.opts.LogMessage != nil {
		return *b.opts.LogMessage
	}
	return LogMessageOptions{}
}

func (b *memoryBus) retryFor(topic string) RetryOptions {
	if cfg, ok := b.opts.Topics[topic]; ok && cfg.Retry != nil {
		return *cfg.Retry
	}
	if b.opts.Retry != nil {
		return *b.opts.Retry
	}
	return RetryOptions{MaxRetries: 3}
}

// Publish 发布业务对象或原始字节。
func (b *memoryBus) Publish(ctx context.Context, topic string, payload any, opts ...PublishOption) error {
	if b.closed.Load() {
		return errors.New("bus is closed")
	}
	o := &PublishOptions{Metadata: map[string]string{}}
	applyPublishOptions(o, opts...)
	var data []byte
	var headers map[string]string
	var key string
	var id = uuid.NewString()
	var eventTime time.Time

	switch v := payload.(type) {
	case *RawEvent:
		if v == nil {
			return errors.New("bus: payload is typed nil *RawEvent")
		}
		// 透传：保留 ID/Key/Headers/Time；逻辑 topic 以函数参数为准（与 Watermill 路径一致）
		if v.ID != "" {
			id = v.ID
		}
		key = v.Key
		if o.MessageKey != "" {
			key = o.MessageKey
		}
		headers = cloneHeaders(v.Headers)
		data = v.Payload
		eventTime = v.Time
	case []byte:
		data = v
		key = o.MessageKey
		headers = map[string]string{}
	case nil:
		data = nil
		key = o.MessageKey
		headers = map[string]string{}
	default:
		m := ResolvePublishMarshaler(b, topic, nil, o.Marshaler)
		bs, err := m.Marshal(payload)
		if err != nil {
			return fmt.Errorf("bus: marshal %q: %w", topic, err)
		}
		data = bs
		key = o.MessageKey
		headers = map[string]string{}
	}
	if headers == nil {
		headers = map[string]string{}
	}
	// 先合并业务 Metadata，再写入协议键，避免覆盖 x-message-key 等
	maps.Copy(headers, o.Metadata)
	for k := range headers {
		if isProtocolMetaKey(k) {
			delete(headers, k)
		}
	}
	// 传播日志属性（白名单），已存在的不覆盖
	for _, k := range b.propagateKeys() {
		if _, ok := headers[k]; ok {
			continue
		}
		for _, a := range logging.AttrsFrom(ctx) {
			if a.Key == k {
				headers[k] = a.Value.String()
				break
			}
		}
	}
	if eventTime.IsZero() {
		eventTime = time.Now()
	}
	ev := &RawEvent{
		ID:      id,
		Topic:   topic,
		Key:     key,
		Headers: headers,
		Payload: data,
		Time:    eventTime,
	}
	if lm := b.logFor(topic); lm.Publish {
		b.logger.DebugContext(ctx, "publishing event", "topic", topic, "key", key)
	}
	return b.dispatch(ctx, ev)
}

func (b *memoryBus) PublishRaw(ctx context.Context, topic string, data []byte, opts ...PublishOption) error {
	return b.Publish(ctx, topic, data, opts...)
}

func (b *memoryBus) dispatch(ctx context.Context, ev *RawEvent) error {
	// 发送必须在 RLock 临界区内完成：与 Stop 的写锁 close(sub.ch) 互斥。
	// 若先拷贝订阅者再解锁发送，关停期间的并发 Publish 会落进
	// "已拷贝、已 close" 窗口，send on closed channel 直接 panic。
	// 非阻塞发送开销极小，持读锁不影响并发 Publish 的吞吐。
	b.mu.RLock()
	defer b.mu.RUnlock()
	subs := b.subs[ev.Topic]
	if len(subs) == 0 {
		return nil
	}
	for _, sub := range subs {
		// 非阻塞投递，满缓冲时丢弃并告警（状态协同不该反压发布者，
		// at-most-once 语义见 Bus 接口注释）；日志带事件 ID 便于对账。
		select {
		case sub.ch <- cloneRawEvent(ev):
		default:
			b.logger.ErrorContext(ctx, "bus dispatch dropped event: subscriber buffer full",
				"topic", ev.Topic, "handler", sub.handlerName, "id", ev.ID)
		}
	}
	return nil
}

// Subscribe 订阅主题，支持 Start 后动态追加（覆盖旧 Broker 的限制）。
func (b *memoryBus) Subscribe(ctx context.Context, topic string, h HandlerFunc, opts ...SubscribeOption) error {
	if h == nil {
		return errors.New("handler is nil")
	}
	o := &SubscribeOptions{}
	applySubscribeOptions(o, opts...)
	handlerName := o.HandlerName
	if handlerName == "" {
		handlerName = topic
	}
	// 合并 Topic 默认值：显式优先
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

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return errors.New("cannot subscribe to a closed bus")
	}
	if _, dup := b.handlerName[handlerName]; dup {
		return fmt.Errorf("duplicate handler name %q", handlerName)
	}
	// 即使同样 topic 的多个 handler 也是允许的，各自独立通道
	ch := make(chan *RawEvent, b.opts.BufferSize)
	subCtx, cancel := context.WithCancel(context.Background())
	sub := &subscriber{
		handlerName: handlerName,
		topic:       topic,
		ch:          ch,
		handler:     h,
		opts:        *o,
		cancel:      cancel,
	}
	b.subs[topic] = append(b.subs[topic], sub)
	b.handlerName[handlerName] = struct{}{}

	// 启动投递 goroutine（独立于 Start 生命周期，跟随订阅）
	go b.loop(subCtx, sub)
	return nil
}

func (b *memoryBus) loop(ctx context.Context, sub *subscriber) {
	retry := b.retryFor(sub.topic)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			b.handleWithRetry(ctx, sub, ev, retry)
		}
	}
}

func (b *memoryBus) handleWithRetry(ctx context.Context, sub *subscriber, ev *RawEvent, retry RetryOptions) {
	// 为 handler 构造带传播属性的 ctx
	hCtx := context.WithValue(ctx, struct{ string }{"x-bus-topic"}, ev.Topic)
	// 还原发布侧日志属性
	existing := map[string]struct{}{}
	for _, a := range logging.AttrsFrom(hCtx) {
		existing[a.Key] = struct{}{}
	}
	var attrs []slog.Attr
	for _, k := range b.propagateKeys() {
		if _, ok := existing[k]; ok {
			continue
		}
		if v, ok := ev.Headers[k]; ok && v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	hCtx = logging.WithAttrs(hCtx, attrs...)

	lm := b.logFor(sub.topic)
	if lm.Subscribe {
		b.logger.DebugContext(hCtx, "received event", "topic", sub.topic, "handler", sub.handlerName)
	}

	// AutoAck：先确认语义在内存 Bus 中等价于不重试
	if sub.opts.AutoAck {
		_ = sub.handler(hCtx, ev)
		return
	}

	var err error
	for attempt := 0; attempt <= retry.MaxRetries; attempt++ {
		err = sub.handler(hCtx, ev)
		if err == nil {
			return
		}
		if sub.opts.ContinueOnError {
			b.logger.ErrorContext(hCtx, "handler failed but continue_on_error is set", "error", err, "handler", sub.handlerName)
			return
		}
		if attempt < retry.MaxRetries {
			if retry.Backoff > 0 {
				select {
				case <-time.After(retry.Backoff):
				case <-ctx.Done():
					return
				}
			}
			b.logger.ErrorContext(hCtx, "handler failed, retrying", "error", err, "attempt", attempt+1, "handler", sub.handlerName)
		}
	}
	b.logger.ErrorContext(hCtx, "handler failed after retries", "error", err, "handler", sub.handlerName)
}

func cloneHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	cp := make(map[string]string, len(h))
	maps.Copy(cp, h)
	return cp
}

func cloneRawEvent(e *RawEvent) *RawEvent {
	cp := *e
	cp.Headers = cloneHeaders(e.Headers)
	if e.Payload != nil {
		cp.Payload = append([]byte(nil), e.Payload...)
	}
	return &cp
}

var _ Bus = (*memoryBus)(nil)
