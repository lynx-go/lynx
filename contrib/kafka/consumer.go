package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/cenkalti/backoff/v5"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
	"github.com/segmentio/kafka-go"
	"github.com/spf13/cast"
)

// ConsumerOptions 是 Kafka 消费者的配置项。
type ConsumerOptions struct {
	Brokers                  []string
	Topic                    string
	Group                    string
	ReaderConfig             *kafka.ReaderConfig
	ErrorCallbackFunc        func(error)
	Instances                int
	LogMessage               bool
	MappedEvent              string
	StillCommitOnBrokerError bool // commit message to kafka while failed forward to broker
}

// HandlerFunc 是 Kafka 消息处理函数。
type HandlerFunc func(ctx context.Context, msg kafka.Message) error

type consumerHandlerWrapper struct {
	h HandlerFunc
}

func (c *consumerHandlerWrapper) HandlerFunc() HandlerFunc {
	return c.h
}

var _ Handler = new(consumerHandlerWrapper)

// Handler 定义 Kafka 消息处理器接口。
type Handler interface {
	HandlerFunc() HandlerFunc
}

// NewConsumer 创建 Kafka 消费者组件；未提供 ReaderConfig 时按 Brokers/Topic/Group 构造。
func NewConsumer(eventName string, broker pubsub.Broker, options ConsumerOptions) *Consumer {
	consumer := &Consumer{
		options:   options,
		eventName: eventName,
		broker:    broker,
	}
	consumer.ctx, consumer.closeCtx = context.WithCancel(context.Background())
	var readerConfig = options.ReaderConfig
	if readerConfig == nil {
		readerConfig = &kafka.ReaderConfig{
			Brokers: options.Brokers,
			Topic:   options.Topic,
			GroupID: options.Group,
		}
	}

	consumer.reader = kafka.NewReader(*readerConfig)
	return consumer
}

// messageReader is the subset of *kafka.Reader used by Consumer.
// It exists as a seam so tests can inject a fake reader.
type messageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Consumer 是 Kafka 消费者组件，消费消息并转发到消息代理。
type Consumer struct {
	app       lynx.App
	options   ConsumerOptions
	reader    messageReader
	eventName string
	broker    pubsub.Broker
	ctx       context.Context
	closeCtx  context.CancelFunc
}

// Name 返回组件名称，格式为 "kafka-consumer-<topic>"。
func (c *Consumer) Name() string {
	return "kafka-consumer-" + c.options.Topic
}

// Init 记录应用实例。
func (c *Consumer) Init(app lynx.App) error {
	c.app = app
	return nil
}

// GetMessageID 从 Kafka 消息头中读取消息 ID。
func GetMessageID(kmsg *kafka.Message) string {
	return getHeader(kmsg.Headers, pubsub.MessageIDKey.String())
}

// NewMessage 将 Kafka 消息转换为 watermill 消息，消息头写入元数据。
func NewMessage(kmsg kafka.Message) *message.Message {
	msgId := GetMessageID(&kmsg)
	msg := message.NewMessage(msgId, kmsg.Value)
	for i := range kmsg.Headers {
		h := kmsg.Headers[i]
		msg.Metadata.Set(h.Key, cast.ToString(h.Value))
	}
	return msg
}

// Start 循环拉取 Kafka 消息并转发到消息代理，拉取失败时按指数退避重试。
func (c *Consumer) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting kafka consumer", "topic", c.options.Topic, "group", c.options.Group, "brokers", c.options.Brokers, "event", c.eventName)
	errorCallback := c.options.ErrorCallbackFunc
	backOff := backoff.NewExponentialBackOff()
	hasError := false
	for {
		select {
		case <-c.ctx.Done():
			return nil
		default:
			msg, err := c.reader.FetchMessage(ctx)
			if err != nil {
				hasError = true
				retryAfter := backOff.NextBackOff()
				log.ErrorContext(ctx, fmt.Sprintf("failed to fetch message, retry after %s", retryAfter.String()), err, "topic", c.options.Topic)
				if errorCallback != nil {
					errorCallback(err)
				}
				time.Sleep(retryAfter)
				continue
			}
			if hasError {
				backOff.Reset()
				hasError = false
			}
			newMsg := NewMessage(msg)
			if c.options.LogMessage {
				log.DebugContext(ctx, "recv kafka message", "message", string(msg.Value), "msg_id", newMsg.UUID, "topic", msg.Topic, "offset", msg.Offset, "partition", msg.Partition)
			}
			if err := c.broker.Publish(ctx, c.eventName, newMsg, pubsub.FromBinder()); err != nil {
				log.ErrorContext(ctx, "failed to publish message to broker", err, "topic", c.options.Topic, "msg_id", newMsg.UUID)
				if !c.options.StillCommitOnBrokerError {
					continue
				}
			}
			if err := c.reader.CommitMessages(ctx, msg); err != nil {
				slog.ErrorContext(ctx, "failed to commit messages", "error", err, "topic", c.options.Topic, "msg_id", newMsg.UUID)
			}

			if c.options.LogMessage {
				log.DebugContext(ctx, "processed kafka message", "topic", msg.Topic, "offset", msg.Offset, "partition", msg.Partition, "msg_id", newMsg.UUID)
			}
		}
	}
}

// Stop 关闭 Kafka reader 并取消消费者上下文。
func (c *Consumer) Stop(ctx context.Context) {
	if err := c.reader.Close(); err != nil {
		slog.ErrorContext(ctx, "Error closing kafka reader", "error", err)
	}
	c.closeCtx()
	log.InfoContext(ctx, "stopped kafka consumer", "event_name", c.eventName)
}

var _ lynx.Component = new(Consumer)

// NewConsumerBuilder 创建 Kafka 消费者构建器。
func NewConsumerBuilder(eventName string, broker pubsub.Broker, options ConsumerOptions) *ConsumerBuilder {
	return &ConsumerBuilder{
		options:   options,
		instances: options.Instances,
		broker:    broker,
		eventName: eventName,
	}
}

// ConsumerBuilder 按 ConsumerOptions 构建消费者组件实例。
type ConsumerBuilder struct {
	options   ConsumerOptions
	instances int
	broker    pubsub.Broker
	eventName string
}

// Build 构建一个新的消费者组件实例。
func (cf *ConsumerBuilder) Build() lynx.Component {
	return NewConsumer(cf.eventName, cf.broker, cf.options)
}

// Options 返回构建参数，包含实例数。
func (cf *ConsumerBuilder) Options() lynx.BuildOptions {
	return lynx.BuildOptions{
		Instances: cf.instances,
	}
}

var _ lynx.ComponentBuilder = new(ConsumerBuilder)
