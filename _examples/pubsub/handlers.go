package main

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/contrib/pubsub"
)

// helloHandler 消费 hello 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到 kafka transport）。
type helloHandler struct{}

func (h *helloHandler) EventName() string   { return "hello" }
func (h *helloHandler) HandlerName() string { return "helloHandler" }

func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		slog.InfoContext(ctx, "hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)

// notifyHandler 订阅 notify 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到内存 transport，不经过 Kafka）。
type notifyHandler struct{}

func (h *notifyHandler) EventName() string   { return "notify" }
func (h *notifyHandler) HandlerName() string { return "notifyHandler" }

func (h *notifyHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		slog.InfoContext(ctx, "notify event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(notifyHandler)
