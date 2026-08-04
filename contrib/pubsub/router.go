package pubsub

import (
	"context"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

// Router 是事件路由组件，负责将注册的 Handler 全部订阅到消息代理。
type Router struct {
	handlers []Handler
	broker   Broker
	ctx      context.Context
	closeCtx context.CancelFunc
}

// Name 返回组件名称 "pubsub-router"。
func (r *Router) Name() string {
	return "pubsub-router"
}

// Init 初始化组件，Router 无需在初始化阶段做额外工作。
func (r *Router) Init(app lynx.Lynx) error {
	return nil
}

// Start 订阅所有事件处理器，并阻塞至组件停止。
func (r *Router) Start(ctx context.Context) error {
	if err := r.Run(ctx); err != nil {
		return err
	}
	<-r.ctx.Done()
	return nil
}

// Stop 取消路由上下文，使 Start 返回。
func (r *Router) Stop(ctx context.Context) {
	r.closeCtx()
}

var _ lynx.Component = (*Router)(nil)

// NewRouter 创建事件路由组件。
func NewRouter(broker Broker, handlers []Handler) *Router {
	ctx, closeCtx := context.WithCancel(context.Background())
	return &Router{
		broker:   broker,
		handlers: handlers,
		ctx:      ctx,
		closeCtx: closeCtx,
	}
}

// Run 将所有事件处理器订阅到消息代理；有 Binder 命中映射时订阅映射后的主题。
func (r *Router) Run(ctx context.Context) error {
	for _, h := range r.handlers {
		log.InfoContext(ctx, "add event handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		var opts []SubscribeOption
		if o, ok := h.(HandlerOptions); ok {
			opts = append(opts, o.Options()...)
		}
		found := false
		for _, binder := range r.broker.Binders() {
			topicName, ok := binder.CanSubscribe(h.EventName())
			if ok {
				if err := r.broker.Subscribe(topicName, h.HandlerName(), h.HandlerFunc(), opts...); err != nil {
					return err
				}
				found = true
			}
		}
		if !found {
			if err := r.broker.Subscribe(h.EventName(), h.HandlerName(), h.HandlerFunc(), opts...); err != nil {
				return err
			}
		}
	}
	return nil
}
