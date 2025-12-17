package kafka

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
	"github.com/segmentio/kafka-go"
	"github.com/spf13/cast"
)

type Producer struct {
	options ProducerOptions
	writer  *kafka.Writer
}

type ProducerOptions struct {
	Brokers      []string
	Topic        string
	WriterConfig *kafka.WriterConfig
	LogMessage   bool
	MappedEvent  string
	BatchSize    int
	BatchTimeout time.Duration
	WriteTimeout time.Duration
	Acks         int
	Async        bool
}

func NewProducer(options ProducerOptions) *Producer {
	var writerConfig = options.WriterConfig
	if writerConfig == nil {
		writerConfig = &kafka.WriterConfig{
			Brokers:      options.Brokers,
			Topic:        options.Topic,
			BatchSize:    options.BatchSize,
			BatchTimeout: options.BatchTimeout,
			Async:        options.Async,
		}
	}
	writer := kafka.NewWriter(*writerConfig)
	return &Producer{
		options: options,
		writer:  writer,
	}
}

func (p *Producer) Produce(ctx context.Context, msgs ...kafka.Message) error {
	if p.options.LogMessage {
		for _, msg := range msgs {
			log.DebugContext(ctx, "sending kafka message", "message", string(msg.Value), "topic", msg.Topic)
		}
	}
	if err := p.writer.WriteMessages(ctx, msgs...); err != nil {
		return err
	}
	if p.options.LogMessage {
		for _, msg := range msgs {
			log.DebugContext(ctx, "sent kafka message", "message", string(msg.Value), "topic", msg.Topic)
		}
	}
	return nil
}

func (p *Producer) Close(ctx context.Context) error {
	return p.writer.Close()
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

func NewKafkaMessage(msg *message.Message, opts ...MessageOption) kafka.Message {
	o := &MessageOptions{}
	for _, opt := range opts {
		opt(o)
	}
	kmsg := kafka.Message{
		Value: msg.Payload,
	}

	if o.Key != "" {
		kmsg.Key = []byte(o.Key)
	}
	kmsg.Headers = []kafka.Header{}
	kmsg.Headers = append(kmsg.Headers, kafka.Header{
		Key:   pubsub.MessageIDKey.String(),
		Value: []byte(msg.UUID),
	})

	if len(o.Headers) > 0 {
		for k, v := range o.Headers {
			kmsg.Headers = append(kmsg.Headers, kafka.Header{
				Key:   k,
				Value: []byte(v),
			})
		}
	}
	for k, v := range msg.Metadata {
		kmsg.Headers = append(kmsg.Headers, kafka.Header{
			Key:   k,
			Value: []byte(cast.ToString(v)),
		})
	}

	return kmsg
}

func NewKafkaMessageJSON(data any, opts ...MessageOption) kafka.Message {
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
