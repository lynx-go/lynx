package pubsub

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
)

// Router 是事件路由组件：Init 期把全部 Handler 缓冲订阅到 Broker。
type Router struct {
	broker   Broker
	handlers []Handler
	logger   *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
}

// Name 返回组件名称 "pubsub-router"。
func (r *Router) Name() string { return "pubsub-router" }

// Init 将全部 Handler 缓冲订阅到 Broker（纯缓冲，无时序依赖）。
func (r *Router) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		r.logger = ctx.Logger("component", "pubsub-router")
	}
	for _, h := range r.handlers {
		r.logger.InfoContext(ctx.Context(), "add event handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		var opts []SubscribeOption
		if o, ok := h.(HandlerOptions); ok {
			opts = append(opts, o.Options()...)
		}
		if err := r.broker.Subscribe(ctx.Context(), h.EventName(), h.HandlerName(), h.HandlerFunc(), opts...); err != nil {
			return err
		}
	}
	return nil
}

// Start 阻塞至组件停止。
func (r *Router) Start(ctx context.Context) error {
	<-r.ctx.Done()
	return nil
}

// Stop 取消路由上下文，使 Start 返回。
func (r *Router) Stop(ctx context.Context) error {
	r.cancel()
	return nil
}

var _ lynx.Component = (*Router)(nil)

// NewRouter 创建事件路由组件。
func NewRouter(broker Broker, handlers []Handler) *Router {
	ctx, cancel := context.WithCancel(context.Background())
	return &Router{broker: broker, handlers: handlers, ctx: ctx, cancel: cancel, logger: slog.Default()}
}
