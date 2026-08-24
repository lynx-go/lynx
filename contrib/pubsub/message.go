package pubsub

import (
	"context"
	"encoding/json"
	"maps"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
)

// Message 是公共 API 的消息类型，与底层 Watermill 解耦。
// ID 是消息唯一标识；Key 是消息键（如 Kafka key）；Headers 承载元数据。
type Message struct {
	ID      string
	Key     string
	Headers map[string]string
	Payload []byte
}

// MessageOption 用于配置 Message 的选项函数。
type MessageOption func(*Message)

// WithID 设置消息 ID。
func WithID(id string) MessageOption {
	return func(m *Message) { m.ID = id }
}

// WithKey 设置消息 key。
func WithKey(key string) MessageOption {
	return func(m *Message) { m.Key = key }
}

// WithHeaders 合并设置消息头。
func WithHeaders(h map[string]string) MessageOption {
	return func(m *Message) {
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		maps.Copy(m.Headers, h)
	}
}

// WithHeader 添加单条消息头。
func WithHeader(k, v string) MessageOption {
	return func(m *Message) {
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		m.Headers[k] = v
	}
}

// NewMessage 创建消息，ID 缺省随机生成。
func NewMessage(payload []byte, opts ...MessageOption) *Message {
	m := &Message{ID: uuid.NewString(), Headers: map[string]string{}, Payload: payload}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

type msgKeyCtx struct{}

func (ctx msgKeyCtx) String() string { return "x-message-key" }

// MessageKeyKey 是消息 key 的 wire 协议键（写入 watermill 元数据 / Kafka header）。
var MessageKeyKey = msgKeyCtx{}

type msgIdCtx struct{}

func (ctx msgIdCtx) String() string { return "x-message-id" }

// MessageIDKey 是消息 ID 的 wire 协议键（写入 Kafka header）。
var MessageIDKey = msgIdCtx{}

// ContextWithMessageID 将消息 ID 写入上下文。
func ContextWithMessageID(ctx context.Context, msgId string) context.Context {
	return context.WithValue(ctx, MessageIDKey, msgId)
}

// MessageIDFromContext 从上下文中获取消息 ID，未设置时返回空字符串。
func MessageIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(MessageIDKey).(string)
	return v
}

// ContextWithMessageKey 将消息 key 写入上下文。
func ContextWithMessageKey(ctx context.Context, msgKey string) context.Context {
	return context.WithValue(ctx, MessageKeyKey, msgKey)
}

// MessageKeyFromContext 从上下文中获取消息 key，未设置时返回空字符串。
func MessageKeyFromContext(ctx context.Context) string {
	v, _ := ctx.Value(MessageKeyKey).(string)
	return v
}

// toWatermill 将公共消息转换为 watermill 消息（内部使用）。
func toWatermill(m *Message) *message.Message {
	wm := message.NewMessage(m.ID, m.Payload)
	if m.Key != "" {
		wm.Metadata.Set(MessageKeyKey.String(), m.Key)
	}
	maps.Copy(wm.Metadata, m.Headers)
	return wm
}

// fromWatermill 将 watermill 消息转换为公共消息（内部使用）。
// key 与协议键 x-message-key 还原为 Message.Key，不进入 Headers。
func fromWatermill(wm *message.Message) *Message {
	m := &Message{
		ID:      wm.UUID,
		Key:     wm.Metadata.Get(MessageKeyKey.String()),
		Headers: map[string]string{},
		Payload: wm.Payload,
	}
	maps.Copy(m.Headers, wm.Metadata)
	delete(m.Headers, MessageKeyKey.String())
	return m
}

// NewJSONMessage 将数据 JSON 序列化后创建消息。
func NewJSONMessage(data any, opts ...MessageOption) (*Message, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return NewMessage(bytes, opts...), nil
}

// MustJSONMessage 将数据 JSON 序列化后创建消息，序列化失败时 panic。
func MustJSONMessage(data any, opts ...MessageOption) *Message {
	msg, err := NewJSONMessage(data, opts...)
	if err != nil {
		panic(err)
	}
	return msg
}
