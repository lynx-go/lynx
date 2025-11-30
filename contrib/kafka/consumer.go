package kafka

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/segmentio/kafka-go"
)

type ConsumerOptions struct {
	Brokers          []string
	Topic            string
	Group            string
	Reader           *kafka.Reader
	ErrorHandlerFunc func(error) error
	Instances        int
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

func NewConsumer(options ConsumerOptions, handler Handler) *Consumer {
	consumer := &Consumer{
		options: options,
		handler: handler,
	}
	if options.Reader != nil {
		consumer.reader = options.Reader
	} else {
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers: options.Brokers,
			Topic:   options.Topic,
		})
		consumer.reader = reader
	}
	return consumer
}

type Consumer struct {
	app     lynx.Lynx
	options ConsumerOptions
	handler Handler
	reader  *kafka.Reader
}

func (c *Consumer) Name() string {
	return "kafka-consumer-" + c.options.Topic
}

func (c *Consumer) Init(app lynx.Lynx) error {
	c.app = app
	return nil
}

func (c *Consumer) Start(ctx context.Context) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if he := c.options.ErrorHandlerFunc; he != nil {
				err = he(err)
				if err != nil {
					return err
				}
			}
			return err
		}
		if h := c.handler.HandlerFunc(); h != nil {
			if err := h(ctx, msg); err != nil {
				if he := c.options.ErrorHandlerFunc; he != nil {
					if err := he(err); err != nil {
						return err
					}
				}
			}
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			slog.ErrorContext(ctx, "Failed to commit messages", err, "topic", c.options.Topic)
		}
	}
}

func (c *Consumer) Stop(ctx context.Context) {
	if err := c.reader.Close(); err != nil {
		slog.ErrorContext(ctx, "Error closing kafka reader", err)
	}
}

var _ lynx.Component = new(Consumer)

func NewConsumerFactory(options ConsumerOptions, handler Handler) *ConsumerFactory {
	return &ConsumerFactory{
		options:   options,
		handler:   handler,
		instances: options.Instances,
	}
}

func NewConsumerFactoryFromHandler(options ConsumerOptions, handler HandlerFunc) *ConsumerFactory {
	return &ConsumerFactory{
		options:   options,
		handler:   &consumerHandlerWrapper{handler},
		instances: options.Instances,
	}
}

type ConsumerFactory struct {
	options   ConsumerOptions
	handler   Handler
	instances int
}

func (cf *ConsumerFactory) Component() lynx.Component {
	return NewConsumer(cf.options, cf.handler)
}

func (cf *ConsumerFactory) Option() lynx.FactoryOption {
	return lynx.FactoryOption{
		Instances: cf.instances,
	}
}

var _ lynx.ComponentFactory = new(ConsumerFactory)
