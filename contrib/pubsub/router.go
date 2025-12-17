package pubsub

import (
	"context"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

type Router struct {
	handlers []Handler
	broker   Broker
	ctx      context.Context
	closeCtx context.CancelFunc
}

func (r *Router) Name() string {
	return "pubsub-router"
}

func (r *Router) Init(app lynx.Lynx) error {
	return nil
}

func (r *Router) Start(ctx context.Context) error {
	if err := r.Run(ctx); err != nil {
		return err
	}
	<-r.ctx.Done()
	return nil
}

func (r *Router) Stop(ctx context.Context) {
	r.closeCtx()
}

var _ lynx.Component = (*Router)(nil)

func NewRouter(broker Broker, handlers []Handler) *Router {
	ctx, closeCtx := context.WithCancel(context.Background())
	return &Router{
		broker:   broker,
		handlers: handlers,
		ctx:      ctx,
		closeCtx: closeCtx,
	}
}

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
