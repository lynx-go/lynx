package pubsub

import (
	"context"

	"github.com/lynx-go/x/log"
)

type Dispatcher struct {
	handlers []Handler
	broker   Broker
}

func NewDispatcher(broker Broker, handlers []Handler) *Dispatcher {
	return &Dispatcher{
		broker:   broker,
		handlers: handlers,
	}
}

func (binder *Dispatcher) Run(ctx context.Context) error {
	for _, h := range binder.handlers {
		log.InfoContext(ctx, "binding handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		if err := binder.broker.Subscribe(h.EventName(), h.HandlerName(), h.HandlerFunc()); err != nil {
			return err
		}
	}
	return nil
}
