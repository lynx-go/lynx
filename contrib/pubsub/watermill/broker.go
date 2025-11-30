package watermill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
	"github.com/spf13/cast"
)

type Options struct {
	Publisher     message.Publisher
	Subscriber    message.Subscriber
	TopicNameFunc TopicNameFunc
	TraceIDKey    string
	TraceIDFunc   TraceIDFunc
}

type TopicNameFunc func(string) string
type TraceIDFunc func(ctx context.Context) string

func NewBroker(opts Options) *Broker {
	return &Broker{
		options:    &opts,
		publisher:  opts.Publisher,
		subscriber: opts.Subscriber,
	}
}

type Broker struct {
	options    *Options
	app        lynx.Lynx
	router     *message.Router
	publisher  message.Publisher
	subscriber message.Subscriber
	brokerId   string
}

func (b *Broker) CheckHealth() error {
	if b.router.IsRunning() {
		return nil
	}
	return errors.New("broker is not running")
}

func (b *Broker) IsRunning() bool {
	return b.router.IsRunning()
}

func (b *Broker) Name() string {
	return "pubsub-watermill"
}

func (b *Broker) Init(app lynx.Lynx) error {
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

func (b *Broker) Start(ctx context.Context) error {
	if err := b.router.Run(ctx); err != nil {
		return err
	}
	return nil
}

func (b *Broker) Stop(ctx context.Context) {
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

func (b *Broker) Publish(ctx context.Context, eventName string, data any, opts ...pubsub.PublishOption) error {
	ctx = context.WithoutCancel(ctx)
	o := &pubsub.PublishOptions{}
	for _, opt := range opts {
		opt(o)
	}
	event := cloudevents.NewEvent()
	event.SetID(uuid.NewString())
	event.SetSource(b.brokerId)
	event.SetType(eventName)
	event.SetTime(time.Now())
	if b.options.TraceIDFunc != nil {
		traceIdKey := "traceId"
		if b.options.TraceIDKey != "" {
			traceIdKey = b.options.TraceIDKey
		}
		traceId := b.options.TraceIDFunc(ctx)
		event.SetExtension(traceIdKey, traceId)
	}
	if err := event.SetData(cloudevents.ApplicationJSON, data); err != nil {
		return err
	}
	topicName := eventName
	if b.options.TopicNameFunc != nil {
		topicName = b.options.TopicNameFunc(eventName)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	msg := message.NewMessageWithContext(ctx, event.ID(), payload)
	if o.MessageKey != "" {
		msg.Metadata.Set("_message_key", o.MessageKey)
	}
	return b.publisher.Publish(topicName, msg)
}

func (b *Broker) Subscribe(eventName, handlerName string, h pubsub.HandlerFunc, opts ...pubsub.SubscribeOption) error {
	topicName := eventName
	if b.options.TopicNameFunc != nil {
		topicName = b.options.TopicNameFunc(eventName)
	}
	o := &pubsub.SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	handler := func(msg *message.Message) error {
		event := cloudevents.NewEvent()
		if err := json.Unmarshal(msg.Payload, &event); err != nil {
			return err
		}
		extensions := event.Extensions()
		traceId := cast.ToString(extensions[strings.ToLower(b.options.TraceIDKey)])
		ctx := pubsub.ContextWithTraceID(msg.Context(), traceId)
		ctx = log.Context(ctx, log.FromContext(ctx), "x-trace-id", traceId)
		return h(ctx, &event)
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

var _ pubsub.Broker = new(Broker)
