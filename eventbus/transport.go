package eventbus

import "context"

// Delivery 是 Transport 订阅侧的一次投递：纯数据信封 + 确认句柄。
// Ack/Nack 由 Bus 在 handler 成功/失败（及 AutoAck）时调用，转达到底层 broker。
// 业务 API（Topic.Subscribe / Event[T]）不出现本类型，也不出现 *message.Message。
type Delivery struct {
	Event *RawEvent
	Ack   func()
	Nack  func()
}

// AckOnce 调用 Ack（nil 时为 no-op）。
func (d Delivery) AckOnce() {
	if d.Ack != nil {
		d.Ack()
	}
}

// NackOnce 调用 Nack（nil 时为 no-op）。
func (d Delivery) NackOnce() {
	if d.Nack != nil {
		d.Nack()
	}
}

// Transport 是 Bus 可插拔的后端，topic 一律为 Transport 侧键（缺省=逻辑名）。
type Transport interface {
	Publish(ctx context.Context, topic string, e *RawEvent) error
	Subscribe(ctx context.Context, topic string, opts SubscribeOptions) (<-chan Delivery, error)
	Topics() []string
	Close() error
}
