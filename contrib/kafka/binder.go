package kafka

import (
	"context"
	"errors"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
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
	consumers map[string]*ConsumerFactory
	producers map[string]*Producer
	stopCh    chan struct{}
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
	return func(ctx context.Context, event pubsub.RawEvent) error {
		return h.broker.Publish(ctx, h.eventName, event)
	}
}

var _ pubsub.Handler = new(binderHandler)

func NewBinder(options BinderOptions, broker pubsub.Broker) *Binder {
	consumers := map[string]*ConsumerFactory{}
	for k, opts := range options.SubscribeOptions {
		h := &binderHandler{}
		consumers[k] = NewConsumerFactoryFromHandler(opts, func(ctx context.Context, msg kafka.Message) error {
			fn := h.HandlerFunc()
			return fn(ctx, msg.Value)
		})
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
		consumers: consumers,
		producers: producers,
	}

	return binder
}

func (b *Binder) ComponentFactories() []lynx.ComponentFactory {
	factories := []lynx.ComponentFactory{}
	for _, factory := range b.consumers {
		factories = append(factories, factory)
	}
	return factories
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
		if err := b.broker.Subscribe(k, k, func(ctx context.Context, event pubsub.RawEvent) error {
			msgKey := pubsub.MessageKeyFromContext(ctx)
			traceId := pubsub.TraceIDFromContext(ctx)
			return producer.Produce(ctx, NewBinaryMessage(event, WithMessageKey(msgKey), WithMessageHeader("x-trace-id", traceId)))
		}); err != nil {
			return err
		}
	}

	<-b.stopCh
	return nil
}

func (b *Binder) Stop(ctx context.Context) {
	b.running = false
	b.stopCh <- struct{}{}
}

var _ pubsub.Binder = new(Binder)
