package pubsub

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lynx-go/lynx"
)

// Router 是事件路由服务：Init 期把全部 Handler 缓冲订阅到 Broker。
type Router struct {
	broker   Broker
	handlers []Handler
	logger   *slog.Logger
}

// Name 返回服务名称 "pubsub-router"。
func (r *Router) Name() string { return "pubsub-router" }

// Init 将全部 Handler 缓冲订阅到 Broker（纯缓冲，无时序依赖）。
// 类型化 handler 的解码目标由其 NewEvent 声明，解码所用 Marshaler 在此
// 按 topic 解析一次（MarshalerFor），处理消息时零额外开销。
// 脱离框架单用时 ctx 可为 nil：logger 与订阅 ctx 均取兜底值。
func (r *Router) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		r.logger = ctx.Logger("service", "pubsub-router")
	}
	initCtx := context.Background()
	if ctx != nil {
		initCtx = ctx.Context()
	}
	for _, h := range r.handlers {
		r.logger.InfoContext(initCtx, "add event handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		var opts []SubscribeOption
		if o, ok := h.(HandlerOptions); ok {
			opts = append(opts, o.Options()...)
		}
		m := r.broker.MarshalerFor(h.EventName())
		if err := r.broker.Subscribe(initCtx, h.EventName(), h.HandlerName(), func(ctx context.Context, event *Message) error {
			ev := h.NewEvent()
			if d, ok := ev.(MessageDecoder); ok {
				if err := d.Decode(m, event); err != nil {
					return fmt.Errorf("pubsub: unmarshal payload: %w", err)
				}
			} else if ev == nil {
				ev = event
			}
			return h.Handle(ctx, ev)
		}, opts...); err != nil {
			return err
		}
	}
	return nil
}

// Start 阻塞至传入 ctx 取消（对齐 schedule/telemetry 的服务契约：
// 框架中断时由 ctx 驱动，Start 不依赖 Stop 的信号）。
func (r *Router) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop 无内部状态需要清理：Start 阻塞在调用方 ctx 上，Stop 无需（也
// 无从）取消；Stop-before-Start 天然安全，直接返回 nil。
func (r *Router) Stop(ctx context.Context) error {
	return nil
}

var _ lynx.Service = (*Router)(nil)

// NewRouter 创建事件路由服务。
func NewRouter(broker Broker, handlers []Handler) *Router {
	return &Router{broker: broker, handlers: handlers, logger: slog.Default()}
}
