// Package kafka 提供 Kafka 绑定组件：Binder、消费者与生产者，
// 通过 pubsub.Broker 接入应用事件总线。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
	"github.com/segmentio/kafka-go"
)

// BinderOptions 是 Kafka Binder 的配置项，按事件名分别配置订阅与发布。
type BinderOptions struct {
	SubscribeOptions map[string]ConsumerOptions
	PublishOptions   map[string]ProducerOptions
}

// Binder 是 Kafka 事件绑定组件，管理 Kafka 消费者与生产者的生命周期。
type Binder struct {
	options                BinderOptions
	broker                 pubsub.Broker
	app                    lynx.App
	running                atomic.Bool
	consumerBuilders       map[string]*ConsumerBuilder
	producers              map[string]*Producer
	ctx                    context.Context
	closeCtx               context.CancelFunc
	publishEventMappings   map[string]string
	subscribeEventMappings map[string]string
}

// CanSubscribe 返回事件名对应的订阅主题及是否存在映射。
func (b *Binder) CanSubscribe(eventName string) (string, bool) {
	topicName, ok := b.subscribeEventMappings[eventName]
	return topicName, ok
}

// SetBroker 注入消息代理实例。
func (b *Binder) SetBroker(broker pubsub.Broker) {
	b.broker = broker
}

// CanPublish 返回事件名对应的发布主题及是否存在映射。
func (b *Binder) CanPublish(eventName string) (string, bool) {
	topicName, ok := b.publishEventMappings[eventName]
	return topicName, ok
}

func getHeader(headers []kafka.Header, key string) string {
	for _, header := range headers {
		if header.Key == key {
			return string(header.Value)
		}
	}
	return ""
}

// NewBinder 创建 Kafka Binder，并根据配置初始化事件与主题的映射。
func NewBinder(options BinderOptions) *Binder {

	binder := &Binder{
		options:                options,
		running:                atomic.Bool{},
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
	log.InfoContext(b.ctx, "get kafka consumer builders")
	builders := []lynx.ComponentBuilder{}
	for _, builder := range b.consumerBuilders {
		builders = append(builders, builder)
	}
	return builders
}

// CheckHealth 实现健康检查，Binder 未运行时返回错误。
func (b *Binder) CheckHealth() error {
	if b.running.Load() {
		return nil
	}
	return errors.New("kafka binder is not running")
}

// Name 返回组件名称 "kafka-binder"。
func (b *Binder) Name() string {
	return "kafka-binder"
}

// Init 初始化 Binder，创建组件上下文并构建消费者与生产者。
func (b *Binder) Init(app lynx.App) error {
	b.app = app
	b.ctx, b.closeCtx = context.WithCancel(app.Context())
	b.initConsumersAndProducers(b.ctx)
	return nil
}

func (b *Binder) initConsumersAndProducers(ctx context.Context) {
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
	log.InfoContext(ctx, "initialized kafka producers and consumers")
}

// Start 为配置了事件映射的生产者订阅对应主题，并阻塞至组件停止。
func (b *Binder) Start(ctx context.Context) error {
	for k := range b.producers {
		producer := b.producers[k]
		eventName := producer.options.MappedEvent
		if eventName == "" {
			eventName = k
		}
		topicName, ok := b.CanPublish(eventName)
		if ok {
			log.InfoContext(ctx, "kafka binder subscribing to topic", "eventName", eventName, "topicName", topicName)
			if err := b.broker.Subscribe(topicName, k+"-ProducerHandler", newProducerHandlerFunc(producer)); err != nil {
				return err
			}
		}
	}
	b.running.CompareAndSwap(false, true)
	<-b.ctx.Done()
	return nil
}

func newProducerHandlerFunc(producer *Producer) pubsub.HandlerFunc {
	return func(ctx context.Context, event *message.Message) error {
		msgKey := pubsub.GetMessageKey(event)
		return producer.Produce(ctx, NewKafkaMessage(event, WithMessageKey(msgKey)))
	}
}

// Stop 关闭所有 Kafka 生产者并取消组件上下文。
func (b *Binder) Stop(ctx context.Context) {
	b.running.CompareAndSwap(true, false)
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

// ToProducerName 返回事件名映射的发布主题名。
func ToProducerName(eventName string) string {
	return fmt.Sprintf("%s:kafka-producer", eventName)
}

// ToConsumerName 返回事件名映射的订阅主题名。
func ToConsumerName(eventName string) string {
	return fmt.Sprintf("%s:kafka-consumer", eventName)
}
