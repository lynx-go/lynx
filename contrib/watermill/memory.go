package watermill

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/lynx-go/lynx/eventbus"
)

// MemoryTransport 是基于 gochannel 的内存 Transport，用于 WatermillBus 的默认回退与本地开发。
type MemoryTransport struct {
	pubSub  messagePubSub
	running atomic.Bool
}

type messagePubSub interface {
	message.Publisher
	message.Subscriber
}

// NewMemoryTransport 创建内存 Transport。
func NewMemoryTransport() *MemoryTransport {
	return &MemoryTransport{
		pubSub: gochannel.NewGoChannel(gochannel.Config{}, watermill.NopLogger{}),
	}
}

// Topics 返回 nil：不声明 topic，仅作默认回退。
func (t *MemoryTransport) Topics() []string { return nil }

// Publish 发布 RawEvent（转换为 watermill 消息）。
func (t *MemoryTransport) Publish(ctx context.Context, topic string, e *eventbus.RawEvent) error {
	msg := toWatermill(e)
	// gochannel's Publish expects topic and messages, no ctx
	return t.pubSub.Publish(topic, msg)
}

// Subscribe 订阅 RawEvent。
func (t *MemoryTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan *eventbus.RawEvent, error) {
	ch, err := t.pubSub.Subscribe(ctx, topic)
	if err != nil {
		return nil, err
	}
	out := make(chan *eventbus.RawEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				raw := fromWatermill(msg)
				raw.Topic = topic
				select {
				case out <- raw:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Close 关闭底层 gochannel。
func (t *MemoryTransport) Close() error {
	err := t.pubSub.Close()
	t.running.Store(false)
	return err
}

// CheckHealth 仅为兼容旧接口，实际健康由 Bus 管理。
func (t *MemoryTransport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("memory transport is not running")
	}
	return nil
}

var _ eventbus.Transport = (*MemoryTransport)(nil)
