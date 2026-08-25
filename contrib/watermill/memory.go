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
	// WK-04：MemoryTransport 不是 Service、没有 Start，首次使用即视为运行，
	// 置位 running 让 CheckHealth 反映真实状态（此前无任何置 true 路径，
	// 导出 API 恒报 not running）。
	t.running.Store(true)
	msg := toWatermill(e)
	return t.pubSub.Publish(topic, msg)
}

// Subscribe 订阅，返回带 Ack/Nack 的 Delivery（转达到底层 gochannel 消息）。
func (t *MemoryTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan eventbus.Delivery, error) {
	// 同 Publish：首次使用置位运行标志（WK-04）。
	t.running.Store(true)
	ch, err := t.pubSub.Subscribe(ctx, topic)
	if err != nil {
		return nil, err
	}
	out := make(chan eventbus.Delivery)
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
				wm := msg
				d := eventbus.Delivery{
					Event: raw,
					Ack:   func() { _ = wm.Ack() },
					Nack:  func() { wm.Nack() },
				}
				select {
				case out <- d:
				case <-ctx.Done():
					wm.Nack()
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

// CheckHealth 反映 Transport 是否处于可用状态：无独立 Start（非 Service），
// 首次 Publish/Subscribe 置位、Close 复位（WK-04）。仅是进程内运行标志，
// 非连通性检查（内存后端无对端可探）。
func (t *MemoryTransport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("memory transport is not running")
	}
	return nil
}

var _ eventbus.Transport = (*MemoryTransport)(nil)
