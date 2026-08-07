package main

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/contrib/pubsub"
)

// HelloEvent 是 hello 逻辑 topic 的业务事件类型。
type HelloEvent struct {
	Message string `json:"message"`
}

// helloHandler 消费 hello 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到 kafka transport），payload 自动反序列化为 HelloEvent，消息
// 元数据经 TypedMessage 信封直接可见。
func helloHandler() pubsub.Handler {
	return pubsub.NewTypedHandler("hello", "helloHandler", func(ctx context.Context, event *pubsub.TypedMessage[HelloEvent]) error {
		slog.InfoContext(ctx, "hello event", "message", event.Payload.Message, "key", event.Key)
		return nil
	})
}

// notifyHandler 订阅 notify 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到内存 transport，不经过 Kafka），直接处理原始消息。
func notifyHandler() pubsub.Handler {
	return pubsub.NewHandler("notify", "notifyHandler", func(ctx context.Context, event *pubsub.Message) error {
		slog.InfoContext(ctx, "notify event", "payload", string(event.Payload))
		return nil
	})
}
