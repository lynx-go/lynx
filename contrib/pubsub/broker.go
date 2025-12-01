package pubsub

import (
	"context"

	"github.com/lynx-go/lynx"
)

type Broker interface {
	lynx.ServerLike
	PubSub
	ID() string
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

func MessageIDFromContext(ctx context.Context) string {
	return ctx.Value(MessageIDKey).(string)
}

func ContextWithMessageID(ctx context.Context, msgId string) context.Context {
	return context.WithValue(ctx, MessageIDKey, msgId)
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

type msgIdCtx struct {
}

func (ctx msgIdCtx) String() string {
	return "x-message-id"
}

var MessageIDKey = msgIdCtx{}

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
	MessageID  string            `json:"message_id"`
	MessageKey string            `json:"message_key"`
	Metadata   map[string]string `json:"metadata"`
}

type PublishOption func(*PublishOptions)

func WithMessageKey(key string) PublishOption {
	return func(opts *PublishOptions) {
		opts.MessageKey = key
	}
}

func WithMessageID(messageId string) PublishOption {
	return func(opts *PublishOptions) {
		opts.MessageID = messageId
	}
}

func WithMetadata(metadata map[string]string) PublishOption {
	return func(opts *PublishOptions) {
		opts.Metadata = metadata
	}
}

func WithMetadataKV(key, value string) PublishOption {
	return func(opts *PublishOptions) {
		if opts.Metadata == nil {
			opts.Metadata = map[string]string{}
		}
		opts.Metadata[key] = value
	}
}
