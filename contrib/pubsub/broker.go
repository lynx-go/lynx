// Package pubsub 提供基于 Watermill 的消息发布订阅抽象：
// Broker 门面、Transport 后端与消息 Handler。
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	// Publish 将消息发布到逻辑 topic；路由表未命中时走默认 Transport。
	Publish(ctx context.Context, topic string, msg *Message, opts ...PublishOption) error
	// Subscribe 注册 topic 的消费 handler。Start 前调用为缓冲注册，
	// Start 后调用返回错误。
	Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
	// Route 显式将 topic 路由到指定 Transport，覆盖自动路由。
	Route(topic string, t Transport)
}

// Options 是 Broker 的配置项。
type Options struct {
	// Transports 参与自动路由：每个 Transport.Topics() 声明的 topic
	// 自动路由到该 Transport；重复声明同一 topic 时 Init 报错。
	Transports []Transport
	// DefaultTransport 承接路由表未命中的 topic。
	DefaultTransport Transport
}

// NewBroker 创建消息代理门面。
func NewBroker(opts Options) *broker {
	return &broker{options: opts, routes: map[string]Transport{}}
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

// Close 关闭底层 Transport，满足 watermill message.Subscriber 接口。
func (a subscriberAdapter) Close() error {
	a.t.Stop(context.Background())
	return nil
}

// Broker 是 Broker 接口的具体实现。
type broker struct {
	options Options
	app     lynx.App
	router  *message.Router
	routes  map[string]Transport
	mu      sync.Mutex
	pending []pendingSubscription
	started bool
}

// Name 返回组件名称 "pubsub-broker"。
func (b *broker) Name() string { return "pubsub-broker" }

// Route 显式将 topic 路由到指定 Transport，覆盖自动路由。
func (b *broker) Route(topic string, t Transport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.routes[topic] = t
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
	router.AddMiddleware(
		middleware.Recoverer,
		middleware.CorrelationID,
		middleware.Retry{MaxRetries: 3}.Middleware,
	)
	router.AddPlugin(plugin.SignalsHandler)
	b.router = router

	for _, t := range b.options.Transports {
		for _, topic := range t.Topics() {
			if prev, ok := b.routes[topic]; ok && prev != t {
				return fmt.Errorf("topic %q is routed to multiple transports", topic)
			}
			b.routes[topic] = t
		}
	}
	return nil
}

func (b *broker) resolve(topic string) (Transport, error) {
	if t, ok := b.routes[topic]; ok {
		return t, nil
	}
	if b.options.DefaultTransport != nil {
		return b.options.DefaultTransport, nil
	}
	return nil, fmt.Errorf("no transport routed for topic %q", topic)
}

// Start 将缓冲订阅统一注册进 watermill router 并运行；任一订阅
// 无归属 Transport 时返回错误。
func (b *broker) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return errors.New("broker already started")
	}
	b.started = true
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()

	for _, p := range pending {
		t, err := b.resolve(p.topic)
		if err != nil {
			return err
		}
		adapter := subscriberAdapter{
			t:    t,
			opts: SubscriptionOptions{Group: p.opts.Group, Instances: p.opts.Instances},
		}
		b.router.AddConsumerHandler(p.handlerName, p.topic, adapter, b.wrapHandler(p.handler, p.opts))
	}
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

// Subscribe 缓冲注册订阅；Start 后调用返回错误。
func (b *broker) Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error {
	o := &SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return errors.New("cannot subscribe to a started broker")
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
		return func(msg *message.Message) error {
			msg.Ack()
			return handler(msg)
		}
	}
	return handler
}

// Publish 将消息发布到逻辑 topic；路由未命中且无默认 Transport 时返回错误。
func (b *broker) Publish(ctx context.Context, topic string, msg *Message, opts ...PublishOption) error {
	o := &PublishOptions{}
	for _, opt := range opts {
		opt(o)
	}
	if o.MessageKey != "" {
		msg.Key = o.MessageKey
	}
	for k, v := range o.Metadata {
		if msg.Headers == nil {
			msg.Headers = map[string]string{}
		}
		msg.Headers[k] = v
	}
	t, err := b.resolve(topic)
	if err != nil {
		return err
	}
	return t.Publish(topic, toWatermill(msg))
}

// SetMessageKey 将消息 key 写入 watermill 消息元数据。
//
// Deprecated: 使用 Message 字段与 WithKey。
func SetMessageKey(msg *message.Message, key string) {
	msg.Metadata.Set(MessageKeyKey.String(), key)
}

// GetMessageKey 从 watermill 消息元数据中读取消息 key。
//
// Deprecated: 使用 Message 字段。
func GetMessageKey(msg *message.Message) string {
	return msg.Metadata.Get(MessageKeyKey.String())
}

// SetMessageID 将消息 ID 写入 watermill 消息元数据。
//
// Deprecated: 使用 Message 字段与 WithID。
func SetMessageID(msg *message.Message, msgId string) {
	msg.Metadata.Set(MessageIDKey.String(), msgId)
}

// GetMessageID 从 watermill 消息元数据中读取消息 ID。
//
// Deprecated: 使用 Message 字段。
func GetMessageID(msg *message.Message) string {
	return msg.Metadata.Get(MessageIDKey.String())
}
