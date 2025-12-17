package pubsub

import (
	"context"
	"errors"
	"fmt"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"go.uber.org/multierr"
)

type Options struct {
	Publisher       message.Publisher
	Subscriber      message.Subscriber
	OnlyFirstBinder bool
}

type TopicNameFunc func(string) string
type TraceIDFunc func(ctx context.Context) string

func NewBroker(opts Options, binders []Binder) Broker {
	b := &broker{
		options:    &opts,
		publisher:  opts.Publisher,
		subscriber: opts.Subscriber,
		binders:    binders,
	}
	for _, binder := range binders {
		binder.SetBroker(b)
	}

	return b
}

type broker struct {
	options    *Options
	app        lynx.Lynx
	router     *message.Router
	publisher  message.Publisher
	subscriber message.Subscriber
	brokerId   string
	binders    []Binder
}

func (b *broker) Binders() []Binder {
	return b.binders
}

func (b *broker) ID() string {
	return b.brokerId
}

func (b *broker) CheckHealth() error {
	if b.router.IsRunning() {
		return nil
	}
	return errors.New("broker is not running")
}

func (b *broker) IsRunning() bool {
	return b.router.IsRunning()
}

func (b *broker) Name() string {
	return "pubsub-watermill"
}

func (b *broker) Init(app lynx.Lynx) error {
	b.app = app
	slogger := b.app.Logger("component", "watermill")
	logger := watermill.NewSlogLogger(slogger)

	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return err
	}
	router.AddMiddleware(
		middleware.Recoverer,
		middleware.CorrelationID,
		middleware.Retry{MaxRetries: 3}.Middleware,
	)
	serverId := lynx.IDFromContext(b.app.Context())
	serviceName := lynx.NameFromContext(b.app.Context())
	b.brokerId = fmt.Sprintf("%s/%s", serviceName, serverId)

	router.AddPlugin(plugin.SignalsHandler)
	b.router = router

	if b.publisher == nil || b.subscriber == nil {
		pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)
		if b.publisher == nil {
			b.publisher = pubSub
		}
		if b.subscriber == nil {
			b.subscriber = pubSub
		}
	}

	return nil

}

func (b *broker) Start(ctx context.Context) error {
	return b.router.Run(ctx)
}

func (b *broker) Stop(ctx context.Context) {
	if err := b.publisher.Close(); err != nil {
		log.ErrorContext(ctx, "error closing publisher", err)
	}
	if err := b.subscriber.Close(); err != nil {
		log.ErrorContext(ctx, "error closing subscriber", err)
	}
	if err := b.router.Close(); err != nil {
		log.ErrorContext(ctx, "error closing router", err)
	}
}

func (b *broker) Publish(ctx context.Context, eventName string, msg *message.Message, opts ...PublishOption) error {
	ctx = context.WithoutCancel(ctx)
	o := &PublishOptions{}
	for _, opt := range opts {
		opt(o)
	}

	msg.Metadata.Set(MessageKeyKey.String(), o.MessageKey)
	for k, v := range o.Metadata {
		msg.Metadata.Set(k, v)
	}
	var errs error
	var found bool
	for _, binder := range b.binders {
		topicName, ok := binder.CanPublish(eventName)
		if ok {
			err := b.publisher.Publish(topicName, msg)
			if err != nil {
				errs = multierr.Append(errs, err)
			}
			found = true
			if b.options.OnlyFirstBinder {
				break
			}
		}
	}
	if !found {
		return b.publisher.Publish(eventName, msg)
	}

	return errs
}

func SetMessageKey(msg *message.Message, key string) {
	msg.Metadata.Set(MessageKeyKey.String(), key)
}

func GetMessageKey(msg *message.Message) string {
	return msg.Metadata.Get(MessageKeyKey.String())
}

func SetMessageID(msg *message.Message, msgId string) {
	msg.Metadata.Set(MessageIDKey.String(), msgId)
}

func GetMessageID(msg *message.Message) string {
	return msg.Metadata.Get(MessageIDKey.String())
}

func (b *broker) Subscribe(eventName, handlerName string, h HandlerFunc, opts ...SubscribeOption) error {
	o := &SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}

	handler := func(msg *message.Message) error {
		msgId := msg.UUID
		ctx := ContextWithMessageID(msg.Context(), msgId)
		ctx = log.Context(ctx, log.FromContext(ctx), MessageIDKey.String(), msgId)

		if err := h(ctx, msg); err != nil {
			log.ErrorContext(ctx, "error handling message", err, "x-message-id", msgId, "eventName", eventName, "handlerName", handlerName)
			if o.ContinueOnError {
				msg.Ack()
				return nil
			}
			msg.Nack()
			return err
		}
		msg.Ack()
		return nil
	}
	if o.AutoAck {
		handler = func(msg *message.Message) error {
			msg.Ack()
			return handler(msg)
		}
	}
	log.InfoContext(context.TODO(), "broker subscribing to topic", "eventName", eventName, "handlerName", handlerName)
	b.router.AddConsumerHandler(handlerName, eventName, b.subscriber, handler)
	if b.router.IsRunning() {
		return b.router.RunHandlers(b.app.Context())
	}
	return nil
}
