package pubsub

import (
	"context"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

type Broker interface {
	lynx.ServerLike
	PubSub
	IsRunning() bool
}

type PubSub interface {
	Publish(ctx context.Context, eventName string, eventData any, opts ...PublishOption) error
	Subscribe(eventName, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
}

type HandlerFunc func(ctx context.Context, event *cloudevents.Event) error

type Handler interface {
	EventName() string
	HandlerName() string
	HandlerFunc() HandlerFunc
}

type BrokerBinder struct {
	handlers []Handler
	broker   Broker
}

func NewBrokerBinder(broker Broker, handlers []Handler) *BrokerBinder {
	return &BrokerBinder{
		broker:   broker,
		handlers: handlers,
	}
}

func (binder *BrokerBinder) Run(ctx context.Context) error {
	for _, h := range binder.handlers {
		log.InfoContext(ctx, "binding handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		if err := binder.broker.Subscribe(h.EventName(), h.HandlerName(), h.HandlerFunc()); err != nil {
			return err
		}
	}
	return nil
}

func TraceIDFromContext(ctx context.Context) string {
	return ctx.Value(traceIDKey).(string)
}

func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

type traceIDCtx struct {
}

var traceIDKey = traceIDCtx{}

type AsyncHandler interface {
	Async() bool
}

type SubscribeOptions struct {
	Async bool `json:"async"`
}

type SubscribeOption func(*SubscribeOptions)

func WithAsync() SubscribeOption {
	return func(opts *SubscribeOptions) {
		opts.Async = true
	}
}

type PublishOptions struct {
	MessageKey string `json:"message_key"`
}

type PublishOption func(*PublishOptions)

func WithMessageKey(key string) PublishOption {
	return func(opts *PublishOptions) {
		opts.MessageKey = key
	}
}
