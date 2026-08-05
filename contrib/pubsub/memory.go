package pubsub

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/lynx-go/lynx"
)

// messagePubSub 是内存 Transport 对底层实现的最小要求：可发布、可订阅、可关闭。
// watermill v1.5.1 已移除 message.PubSub 组合接口，此处按需本地组合。
type messagePubSub interface {
	message.Publisher
	message.Subscriber
}

// MemoryTransport 是进程内 Transport，基于 watermill gochannel。
type MemoryTransport struct {
	pubSub  messagePubSub
	running atomic.Bool
}

// NewMemoryTransport 创建进程内 Transport，可作 Broker 的默认 Transport。
func NewMemoryTransport() *MemoryTransport {
	return &MemoryTransport{
		pubSub: gochannel.NewGoChannel(gochannel.Config{}, watermill.NopLogger{}),
	}
}

// Name 返回组件名称 "pubsub-memory"。
func (t *MemoryTransport) Name() string { return "pubsub-memory" }

// Init 无额外初始化工作。
func (t *MemoryTransport) Init(app lynx.App) error { return nil }

// Start 标记运行并阻塞至 ctx 取消。
func (t *MemoryTransport) Start(ctx context.Context) error {
	t.running.Store(true)
	<-ctx.Done()
	t.running.Store(false)
	return nil
}

// Stop 关闭底层 gochannel。
func (t *MemoryTransport) Stop(ctx context.Context) {
	if err := t.pubSub.Close(); err != nil {
		// gochannel.Close 在正常关闭时无错误。
	}
	t.running.Store(false)
}

// CheckHealth 报告 Transport 是否在运行。
func (t *MemoryTransport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("memory transport is not running")
	}
	return nil
}

// Publish 发布消息到逻辑 topic（gochannel 按 topic 精确匹配）。
func (t *MemoryTransport) Publish(ctx context.Context, topic string, msgs ...*message.Message) error {
	return t.pubSub.Publish(topic, msgs...)
}

// Subscribe 订阅逻辑 topic；SubscriptionOptions 对内存 Transport 无意义。
func (t *MemoryTransport) Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error) {
	return t.pubSub.Subscribe(ctx, topic)
}

// Topics 返回 nil：内存 Transport 不声明 topic，仅作默认回退。
func (t *MemoryTransport) Topics() []string { return nil }
