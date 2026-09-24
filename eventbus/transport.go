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
// 消费组 / 消费者成员数等后端特有概念由各 Transport 自己的配置承担
//（如 kafka consumer.group_id / instances），不进 Bus 层。
//
// 生命周期归属契约：Transport 独立于 Bus 生存——Bus.Stop 只关闭自身与内置
// 的生命周期内存后端，不关闭用户传入的 Transport。需要框架托管 Init/Start/
// Stop 顺序与健康检查时，实现方应同时实现 lynx.Service（及可选 lynx.Checker）
// 并由应用 Register（先例：contrib/watermill-kafka 的 Transport）。
type Transport interface {
	Publish(ctx context.Context, topic string, e *RawEvent) error
	// Subscribe 订阅 Transport 侧键；opts 当前保留为后端扩展缝（Bus 不填
	// 后端特有字段——消费组 / 成员数由 Transport 自己的配置决定）。
	Subscribe(ctx context.Context, topic string, opts SubscribeOptions) (<-chan Delivery, error)
	Topics() []string
	Close() error
}
