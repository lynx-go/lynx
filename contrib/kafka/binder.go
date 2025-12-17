package kafka

import (
	"context"
	"errors"
	"fmt"

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
	options                BinderOptions
	broker                 pubsub.Broker
	app                    lynx.Lynx
	running                bool
	consumerBuilders       map[string]*ConsumerBuilder
	producers              map[string]*Producer
	ctx                    context.Context
	closeCtx               context.CancelFunc
	publishEventMappings   map[string]string
	subscribeEventMappings map[string]string
}

func (b *Binder) CanSubscribe(eventName string) (string, bool) {
	topicName, ok := b.subscribeEventMappings[eventName]
	return topicName, ok
}

func (b *Binder) SetBroker(broker pubsub.Broker) {
	b.broker = broker
}

func (b *Binder) CanPublish(eventName string) (string, bool) {
	topicName, ok := b.publishEventMappings[eventName]
	return topicName, ok
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

func NewBinder(options BinderOptions) *Binder {

	binder := &Binder{
		options:                options,
		running:                false,
		subscribeEventMappings: map[string]string{},
		publishEventMappings:   map[string]string{},
	}
	for _, sub := range options.SubscribeOptions {
		if sub.MappedEvent != "" {
			binder.subscribeEventMappings[sub.MappedEvent] = ToConsumerName(sub.MappedEvent)
		}
	}

	for _, pub := range options.PublishOptions {
		if pub.MappedEvent != "" {
			binder.publishEventMappings[pub.MappedEvent] = ToProducerName(pub.MappedEvent)
		}
	}

	return binder
}

// ConsumerBuilders 获取 Consumer 构造器
// 因为 binder 中需要先在 Init() 中初始化 consumer builders，所以 binder.ConsumerBuilders() 不能和 binder 同时注入
func (b *Binder) ConsumerBuilders() []lynx.ComponentBuilder {
	builders := []lynx.ComponentBuilder{}
	for _, builder := range b.consumerBuilders {
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
	b.ctx, b.closeCtx = context.WithCancel(app.Context())

	builders := map[string]*ConsumerBuilder{}
	for k, opts := range b.options.SubscribeOptions {
		eventName := opts.MappedEvent
		if eventName == "" {
			eventName = k
		}
		builders[k] = NewConsumerBuilder(eventName, b.broker, opts)
	}
	b.consumerBuilders = builders

	producers := map[string]*Producer{}
	for k, opts := range b.options.PublishOptions {
		producer := NewProducer(opts)
		producers[k] = producer
	}
	b.producers = producers
	return nil
}

func (b *Binder) Start(ctx context.Context) error {
	b.running = true

	for k := range b.producers {
		producer := b.producers[k]
		eventName := producer.options.MappedEvent
		if eventName == "" {
			eventName = k
		}
		topicName, ok := b.CanPublish(eventName)
		if ok {
			log.InfoContext(ctx, "binder subscribing to topic", "eventName", eventName, "topicName", topicName)
			if err := b.broker.Subscribe(topicName, k, func(ctx context.Context, event *message.Message) error {
				msgKey := pubsub.GetMessageKey(event)
				return producer.Produce(ctx, NewKafkaMessage(event, WithMessageKey(msgKey)))
			}); err != nil {
				return err
			}
		}
	}
	<-b.ctx.Done()
	return nil
}

func (b *Binder) Stop(ctx context.Context) {
	b.running = false
	for k, producer := range b.producers {
		log.InfoContext(ctx, "close kafka producer", "event_name", k)
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
	return fmt.Sprintf("%s:kafka-producer", eventName)
}

func ToConsumerName(eventName string) string {
	return fmt.Sprintf("%s:kafka-consumer", eventName)
}
