package kafka

import (
	"context"
	"encoding/json"

	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/segmentio/kafka-go"
)

type Producer struct {
	options ProducerOptions
	writer  *kafka.Writer
}

type ProducerOptions struct {
	Brokers []string
	Topic   string
	Writer  *kafka.Writer
}

func NewProducer(options ProducerOptions) *Producer {
	var writer *kafka.Writer
	if options.Writer != nil {
		writer = options.Writer
	} else {
		writer = kafka.NewWriter(kafka.WriterConfig{
			Brokers: options.Brokers,
			Topic:   options.Topic,
		})
	}
	return &Producer{
		options: options,
		writer:  writer,
	}
}

func (p *Producer) Produce(ctx context.Context, msgs ...kafka.Message) error {
	return p.writer.WriteMessages(ctx, msgs...)
}

type MessageOptions struct {
	Key     string
	Headers map[string]string
}

type MessageOption func(*MessageOptions)

func WithMessageKey(key string) MessageOption {
	return func(o *MessageOptions) {
		o.Key = key
	}
}

func WithMessageHeaders(headers map[string]string) MessageOption {
	return func(o *MessageOptions) {
		o.Headers = headers
	}
}

func WithMessageHeader(key, value string) MessageOption {
	return func(o *MessageOptions) {
		if o.Headers == nil {
			o.Headers = map[string]string{}
		}
		o.Headers[key] = value
	}
}

func NewBinaryMessage(data pubsub.RawEvent, opts ...MessageOption) kafka.Message {
	o := &MessageOptions{}
	for _, opt := range opts {
		opt(o)
	}
	msg := kafka.Message{
		Value: data,
	}

	if o.Key != "" {
		msg.Key = []byte(o.Key)
	}
	if len(o.Headers) > 0 {
		msg.Headers = []kafka.Header{}
		for k, v := range o.Headers {
			msg.Headers = append(msg.Headers, kafka.Header{
				Key:   k,
				Value: []byte(v),
			})
		}
	}

	return msg
}

func NewJSONMessage(data any, opts ...MessageOption) kafka.Message {
	o := &MessageOptions{}
	for _, opt := range opts {
		opt(o)
	}
	bytes, _ := json.Marshal(data)
	msg := kafka.Message{
		Value: bytes,
	}

	if o.Key != "" {
		msg.Key = []byte(o.Key)
	}
	if len(o.Headers) > 0 {
		msg.Headers = []kafka.Header{}
		for k, v := range o.Headers {
			msg.Headers = append(msg.Headers, kafka.Header{
				Key:   k,
				Value: []byte(v),
			})
		}
	}

	return msg
}
