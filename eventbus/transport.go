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

// DeliveryMode 声明后端的投递模式：多个订阅者共存时消息如何分配。
// Bus 据此决定消费组占用检查（WK-01）——模式是每个后端必答的内在属性，
// 不是可选能力（可选能力见 DefaultGrouper / cluster.TTLAware 一类断言式接口）。
type DeliveryMode uint8

const (
	// DeliveryBroadcast 广播：每个订阅者收到全部消息（进程内内存后端）。
	DeliveryBroadcast DeliveryMode = iota
	// DeliveryConsumerGroup 消费组：同组订阅者瓜分消息（Kafka 等分区后端）。
	// 两个 handler 共用同一 topic+组会静默各收一半，Bus 在订阅期显式拒绝；
	// 广播语义用互不相同的组，竞争消费用单 handler + WithInstances。
	DeliveryConsumerGroup
)

// String 使投递模式在错误信息与日志中可读。
func (m DeliveryMode) String() string {
	switch m {
	case DeliveryBroadcast:
		return "broadcast"
	case DeliveryConsumerGroup:
		return "consumer-group"
	default:
		return "unknown"
	}
}

// Transport 是 Bus 可插拔的后端，topic 一律为 Transport 侧键（缺省=逻辑名）。
//
// 生命周期归属契约：Transport 独立于 Bus 生存——Bus.Stop 只关闭自身与内置
// 的生命周期内存后端，不关闭用户传入的 Transport。需要框架托管 Init/Start/
// Stop 顺序与健康检查时，实现方应同时实现 lynx.Service（及可选 lynx.Checker）
// 并由应用 Register（先例：contrib/watermill-kafka 的 Transport）。
type Transport interface {
	Publish(ctx context.Context, topic string, e *RawEvent) error
	Subscribe(ctx context.Context, topic string, opts SubscribeOptions) (<-chan Delivery, error)
	Topics() []string
	Close() error
	// DeliveryMode 声明投递模式（Broadcast / ConsumerGroup），Bus 据此启用
	// 消费组占用检查。
	DeliveryMode() DeliveryMode
}

// DefaultGrouper 是 DeliveryConsumerGroup 后端的可选能力：返回订阅键的
// 配置默认组（如 kafka consumer.group_id），无默认组时 ok=false。
// Bus 据此计算有效组（显式 WithGroup 为空时取默认），使"某 handler 显式
// 指定的组恰好等于另一 handler 留空的默认组"也能被占用检查识别。
type DefaultGrouper interface {
	DefaultGroup(key string) (group string, ok bool)
}
