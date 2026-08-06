package pubsub

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
)

// Transport 是消息代理后端：一个后端（kafka/内存/未来 redis-stream 等）
// 对应一个 Transport 组件。topic 参数一律是逻辑名，物理名解析在实现内部。
type Transport interface {
	lynx.Service
	// Publish 将消息发布到逻辑 topic；ctx 用于传播 trace/应用元数据。
	Publish(ctx context.Context, topic string, msgs ...*message.Message) error
	// Subscribe 订阅逻辑 topic，opts 携带订阅参数（代码显式值优先于
	// Transport 自身配置）。返回的 channel 在取消时关闭。
	Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error)
	// Topics 返回该 Transport 承接的逻辑 topic 全集（Broker 自动路由用）。
	Topics() []string
}

// SubscriptionOptions 是订阅参数；Group 为空字符串、Instances 小于 1 时
// 由 Transport 按自身配置兜底。
type SubscriptionOptions struct {
	Group     string
	Instances int
}
