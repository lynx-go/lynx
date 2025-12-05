package kafka

import (
	"context"
	"errors"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
	"github.com/segmentio/kafka-go"
)

type BinderOptions struct {
	SubscribeOptions map[string]ConsumerOptions
	PublishOptions   map[string]ProducerOptions
}

type Binder struct {
	options   BinderOptions
	broker    pubsub.Broker
	app       lynx.Lynx
	running   bool
	builders  map[string]*ConsumerBuilder
	producers map[string]*Producer
	ctx       context.Context
	closeCtx  context.CancelFunc
}

type binderHandler struct {
	eventName string
	broker    pubsub.Broker
}

func (h *binderHandler) EventName() string {
	return h.eventName
}

func (h *binderHandler) HandlerName() string {
	return h.eventName
}

func (h *binderHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *message.Message) error {
		return h.broker.Publish(ctx, h.eventName, event)
	}
}

var _ pubsub.Handler = new(binderHandler)

func getHeader(headers []kafka.Header, key string) string {
	for _, header := range headers {
		if header.Key == key {
			return string(header.Value)
		}
	}
	return ""
}

func NewBinder(options BinderOptions, broker pubsub.Broker) *Binder {
	builders := map[string]*ConsumerBuilder{}
	for k, opts := range options.SubscribeOptions {
		builders[k] = NewConsumerBuilder(k, broker, opts)
	}
	producers := map[string]*Producer{}
	for k, opts := range options.PublishOptions {
		producer := NewProducer(opts)
		producers[k] = producer
	}

	binder := &Binder{
		options:   options,
		broker:    broker,
		running:   false,
		builders:  builders,
		producers: producers,
	}

	binder.ctx, binder.closeCtx = context.WithCancel(context.TODO())
	return binder
}

func (b *Binder) Builders() []lynx.ComponentBuilder {
	builders := []lynx.ComponentBuilder{}
	for _, builder := range b.builders {
		builders = append(builders, builder)
	}
	return builders
}

func (b *Binder) CheckHealth() error {
	if b.running {
		return nil
	}
	return errors.New("kafka binder is not running")
}

func (b *Binder) Name() string {
	return "kafka-binder"
}

func (b *Binder) Init(app lynx.Lynx) error {
	b.app = app
	return nil
}

func (b *Binder) Start(ctx context.Context) error {
	b.running = true

	for k := range b.producers {
		producer := b.producers[k]
		if err := b.broker.Subscribe(ToProducerName(k), k, func(ctx context.Context, event *message.Message) error {
			msgKey := pubsub.GetMessageKey(event)
			return producer.Produce(ctx, NewKafkaMessage(event, WithMessageKey(msgKey)))
		}); err != nil {
			return err
		}
	}
	<-b.ctx.Done()
	return nil
}

func (b *Binder) Stop(ctx context.Context) {
	b.running = false
	for k, producer := range b.producers {
		err := producer.Close(ctx)
		if err != nil {
			log.ErrorContext(ctx, "failed to close producer", err, "producer", k)
		}
	}

	b.closeCtx()
	log.InfoContext(ctx, "kafka binder stopped")
}

var _ pubsub.Binder = new(Binder)

func ToProducerName(eventName string) string {
	return "producer:" + eventName
}

func ToConsumerName(eventName string) string {
	return "consumer:" + eventName
}
