package pubsub

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/encoding/json"
)

type Broker interface {
	lynx.ServerLike
	PubSub
	ID() string
	IsRunning() bool
}

type PubSub interface {
	Publish(ctx context.Context, eventName string, message *message.Message, opts ...PublishOption) error
	Subscribe(eventName, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
}

type RawEvent []byte

type HandlerFunc func(ctx context.Context, event *message.Message) error

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
	v, _ := ctx.Value(MessageKeyKey).(string)
	return v
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
	MessageKey string            `json:"message_key"`
	Metadata   map[string]string `json:"metadata"`
}

type PublishOption func(*PublishOptions)

func WithMessageKey(key string) PublishOption {
	return func(opts *PublishOptions) {
		opts.MessageKey = key
	}
}

func WithMetadata(metadata map[string]string) PublishOption {
	return func(opts *PublishOptions) {
		opts.Metadata = metadata
	}
}

func WithMetadataField(key, value string) PublishOption {
	return func(opts *PublishOptions) {
		if opts.Metadata == nil {
			opts.Metadata = map[string]string{}
		}
		opts.Metadata[key] = value
	}
}

func NewJSONMessage(data any) *message.Message {
	bytes := json.MustMarshal(data)
	return message.NewMessage(uuid.NewString(), bytes)
}
