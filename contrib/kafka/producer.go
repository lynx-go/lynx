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

// messageWriter is the subset of *kafka.Writer used by Producer.
// It exists as a seam so tests can inject a fake writer.
type messageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Producer 封装 kafka.Writer，负责向 Kafka 写入消息。
type Producer struct {
	options ProducerOptions
	writer  messageWriter
}

// ProducerOptions 是 Kafka 生产者的配置项。
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

// NewProducer 创建 Kafka 生产者；未提供 WriterConfig 时按配置项构造。
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

// Produce 将消息写入 Kafka，写入失败时返回错误。
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

// Close 关闭底层 kafka.Writer。
func (p *Producer) Close(ctx context.Context) error {
	return p.writer.Close()
}

// MessageOptions 是 Kafka 消息的配置项。
type MessageOptions struct {
	Key     string
	Headers map[string]string
}

// MessageOption 用于配置 MessageOptions 的选项函数。
type MessageOption func(*MessageOptions)

// WithMessageKey 设置 Kafka 消息的 key。
func WithMessageKey(key string) MessageOption {
	return func(o *MessageOptions) {
		o.Key = key
	}
}

// WithMessageHeaders 设置 Kafka 消息头。
func WithMessageHeaders(headers map[string]string) MessageOption {
	return func(o *MessageOptions) {
		o.Headers = headers
	}
}

// WithMessageHeader 添加单个 Kafka 消息头。
func WithMessageHeader(key, value string) MessageOption {
	return func(o *MessageOptions) {
		if o.Headers == nil {
			o.Headers = map[string]string{}
		}
		o.Headers[key] = value
	}
}

// NewKafkaMessage 将 watermill 消息转换为 Kafka 消息，消息元数据写入消息头。
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

// NewKafkaMessageJSON 将数据 JSON 序列化后构造 Kafka 消息。
func NewKafkaMessageJSON(data any, opts ...MessageOption) (kafka.Message, error) {
	o := &MessageOptions{}
	for _, opt := range opts {
		opt(o)
	}
	bytes, err := json.Marshal(data)
	if err != nil {
		return kafka.Message{}, err
	}
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

	return msg, nil
}
