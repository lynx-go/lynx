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
	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

type Options struct {
	Publisher     message.Publisher
	Subscriber    message.Subscriber
	TopicNameFunc TopicNameFunc
	TraceIDFunc   TraceIDFunc
}

type TopicNameFunc func(string) string
type TraceIDFunc func(ctx context.Context) string

func NewBroker(opts Options) Broker {
	return &broker{
		options:    &opts,
		publisher:  opts.Publisher,
		subscriber: opts.Subscriber,
	}
}

type broker struct {
	options    *Options
	app        lynx.Lynx
	router     *message.Router
	publisher  message.Publisher
	subscriber message.Subscriber
	brokerId   string
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
	slogger := b.app.Logger("category", "pubsub-watermill")
	logger := watermill.NewSlogLogger(slogger)

	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return err
	}
	router.AddMiddleware(
		middleware.Recoverer,
		middleware.CorrelationID,
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

	var msgId = o.MessageID
	if msgId == "" {
		if b.options.TraceIDFunc != nil {
			msgId = b.options.TraceIDFunc(ctx)
		} else {
			msgId = uuid.NewString()
		}
	}
	topicName := eventName
	if b.options.TopicNameFunc != nil {
		topicName = b.options.TopicNameFunc(eventName)
	}
	msg.UUID = msgId
	msg.Metadata.Set(MessageKeyKey.String(), o.MessageKey)
	if msgId != "" {
		msg.Metadata.Set(MessageIDKey.String(), msgId)
	}

	return b.publisher.Publish(topicName, msg)
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
	topicName := eventName
	if b.options.TopicNameFunc != nil {
		topicName = b.options.TopicNameFunc(eventName)
	}
	o := &SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	handler := func(msg *message.Message) error {
		msgId := msg.Metadata[MessageIDKey.String()]
		ctx := ContextWithMessageID(msg.Context(), msgId)
		ctx = log.Context(ctx, log.FromContext(ctx), MessageIDKey.String(), msgId)

		return h(ctx, msg)
	}
	if o.Async {
		handler = func(msg *message.Message) error {
			msg.Ack()
			return handler(msg)
		}
	}
	b.router.AddConsumerHandler(handlerName, topicName, b.subscriber, handler)
	if b.router.IsRunning() {
		return b.router.RunHandlers(b.app.Context())
	}
	return nil
}
