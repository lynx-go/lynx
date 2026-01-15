package kafka

import (
	"context"
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

type ConsumerOptions struct {
	Brokers          []string
	Topic            string
	Group            string
	ReaderConfig     *kafka.ReaderConfig
	ErrorHandlerFunc func(error) error
	Instances        int
	LogMessage       bool
	MappedEvent      string
}

type HandlerFunc func(ctx context.Context, msg kafka.Message) error

type consumerHandlerWrapper struct {
	h HandlerFunc
}

func (c *consumerHandlerWrapper) HandlerFunc() HandlerFunc {
	return c.h
}

var _ Handler = new(consumerHandlerWrapper)

type Handler interface {
	HandlerFunc() HandlerFunc
}

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

type Consumer struct {
	app       lynx.Lynx
	options   ConsumerOptions
	reader    *kafka.Reader
	eventName string
	broker    pubsub.Broker
	ctx       context.Context
	closeCtx  context.CancelFunc
}

func (c *Consumer) Name() string {
	return "kafka-consumer-" + c.options.Topic
}

func (c *Consumer) Init(app lynx.Lynx) error {
	c.app = app
	return nil
}

func GetMessageID(kmsg *kafka.Message) string {
	return getHeader(kmsg.Headers, pubsub.MessageIDKey.String())
}

func NewMessage(kmsg kafka.Message) *message.Message {
	msgId := GetMessageID(&kmsg)
	msg := message.NewMessage(msgId, kmsg.Value)
	for i := range kmsg.Headers {
		h := kmsg.Headers[i]
		msg.Metadata.Set(h.Key, cast.ToString(h.Value))
	}
	return msg
}

func (c *Consumer) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting kafka consumer", "topic", c.options.Topic, "group", c.options.Group, "brokers", c.options.Brokers, "event", c.eventName)
	errorHandlerFunc := c.options.ErrorHandlerFunc
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
				if errorHandlerFunc != nil {
					if err := errorHandlerFunc(err); err != nil {
						return err
					}
				} else {
					log.ErrorContext(ctx, "failed to fetch message", err, "topic", c.options.Topic)
				}
				time.Sleep(backOff.NextBackOff())
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
			if err := c.broker.Publish(ctx, c.eventName, NewMessage(msg), pubsub.WithFromBinder()); err != nil {
				if errorHandlerFunc != nil {
					if err := errorHandlerFunc(err); err != nil {
						return err
					}
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

func (c *Consumer) Stop(ctx context.Context) {
	if err := c.reader.Close(); err != nil {
		slog.ErrorContext(ctx, "Error closing kafka reader", err)
	}
	c.closeCtx()
	log.InfoContext(ctx, "stopped kafka consumer", "event_name", c.eventName)
}

var _ lynx.Component = new(Consumer)

func NewConsumerBuilder(eventName string, broker pubsub.Broker, options ConsumerOptions) *ConsumerBuilder {
	return &ConsumerBuilder{
		options:   options,
		instances: options.Instances,
		broker:    broker,
		eventName: eventName,
	}
}

type ConsumerBuilder struct {
	options   ConsumerOptions
	instances int
	broker    pubsub.Broker
	eventName string
}

func (cf *ConsumerBuilder) Build() lynx.Component {
	return NewConsumer(cf.eventName, cf.broker, cf.options)
}

func (cf *ConsumerBuilder) Options() lynx.BuildOptions {
	return lynx.BuildOptions{
		Instances: cf.instances,
	}
}

var _ lynx.ComponentBuilder = new(ConsumerBuilder)
