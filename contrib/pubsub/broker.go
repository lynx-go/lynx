package pubsub

import (
	"context"

	"github.com/lynx-go/lynx"
)

type Broker interface {
	lynx.ServerLike
	PubSub
	IsRunning() bool
}

type PubSub interface {
	Publish(ctx context.Context, eventName string, eventData RawEvent, opts ...PublishOption) error
	Subscribe(eventName, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
}

type RawEvent []byte

type HandlerFunc func(ctx context.Context, event RawEvent) error

type Handler interface {
	EventName() string
	HandlerName() string
	HandlerFunc() HandlerFunc
}

func TraceIDFromContext(ctx context.Context) string {
	return ctx.Value(TraceIDKey).(string)
}

func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, TraceIDKey, traceID)
}

type msgKeyCtx struct {
}

func (ctx msgKeyCtx) String() string {
	return "x-message-key"
}

var MessageKeyKey = msgKeyCtx{}

func ContextWithMessageKey(ctx context.Context, msgKey string) context.Context {
	return context.WithValue(ctx, MessageKeyKey, msgKey)
}

func MessageKeyFromContext(ctx context.Context) string {
	return ctx.Value(MessageKeyKey).(string)
}

type traceIDCtx struct {
}

func (ctx traceIDCtx) String() string {
	return "x-trace-id"
}

var TraceIDKey = traceIDCtx{}

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
