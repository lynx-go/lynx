package pubsub

import (
	"context"

	"github.com/lynx-go/x/log"
)

type Router struct {
	handlers []Handler
	broker   Broker
}

func NewRouter(broker Broker, handlers []Handler) *Router {
	return &Router{
		broker:   broker,
		handlers: handlers,
	}
}

func (r *Router) Run(ctx context.Context) error {
	for _, h := range r.handlers {
		log.InfoContext(ctx, "add event handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		if err := r.broker.Subscribe(h.EventName(), h.HandlerName(), h.HandlerFunc()); err != nil {
			return err
		}
	}
	return nil
}
