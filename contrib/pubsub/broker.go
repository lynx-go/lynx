// Package pubsub 提供基于 Watermill 的消息发布订阅抽象：
// Broker、Binder、Router 与消息 Handler。
package pubsub

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/encoding/json"
)

// Broker 是消息代理组件接口，统一管理发布订阅与绑定的 Binder。
type Broker interface {
	lynx.ServerLike
	PubSub
	ID() string
	IsRunning() bool
	Binders() []Binder
}

// PubSub 定义消息发布与订阅接口。
type PubSub interface {
	Publish(ctx context.Context, topicName string, message *message.Message, opts ...PublishOption) error
	Subscribe(topicName, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
}

// RawEvent 是未解码的原始事件数据。
type RawEvent []byte

// HandlerFunc 是事件处理函数，返回错误时按订阅选项决定重试或确认。
type HandlerFunc func(ctx context.Context, event *message.Message) error

// Handler 定义事件处理器的元信息与处理函数。
type Handler interface {
	EventName() string
	HandlerName() string
	HandlerFunc() HandlerFunc
}

// HandlerOptions 可为 Handler 附加订阅选项。
type HandlerOptions interface {
	Options() []SubscribeOption
}

// SubscribeOptions 是订阅行为的配置项。
type SubscribeOptions struct {
	AutoAck         bool `json:"auto_ack"`
	ContinueOnError bool `json:"continue_on_error"`
}

// SubscribeOption 用于配置 SubscribeOptions 的选项函数。
type SubscribeOption func(*SubscribeOptions)

// WithAutoAck 设置订阅为自动确认：消息到达即 Ack，处理失败不影响确认。
func WithAutoAck() SubscribeOption {
	return func(opts *SubscribeOptions) {
		opts.AutoAck = true
	}
}

// WithContinueOnError 设置处理失败时仍确认消息，不再重投。
func WithContinueOnError() SubscribeOption {
	return func(opts *SubscribeOptions) {
		opts.ContinueOnError = true
	}
}

// PublishOptions 是发布行为的配置项。
type PublishOptions struct {
	MessageKey string            `json:"message_key"`
	Metadata   map[string]string `json:"metadata"`
	FromBinder bool              `json:"from_binder"`
}

// PublishOption 用于配置 PublishOptions 的选项函数。
type PublishOption func(*PublishOptions)

// FromBinder 标记消息来自 Binder 转发，发布时跳过 Binder 事件映射。
func FromBinder() PublishOption {
	return func(opts *PublishOptions) {
		opts.FromBinder = true
	}
}

// WithMessageKey 设置消息 key，发布时写入消息元数据。
func WithMessageKey(key string) PublishOption {
	return func(opts *PublishOptions) {
		opts.MessageKey = key
	}
}

// WithMetadata 设置消息元数据，发布时合并进消息。
func WithMetadata(metadata map[string]string) PublishOption {
	return func(opts *PublishOptions) {
		opts.Metadata = metadata
	}
}

// WithMetadataField 添加单条消息元数据字段。
func WithMetadataField(key, value string) PublishOption {
	return func(opts *PublishOptions) {
		if opts.Metadata == nil {
			opts.Metadata = map[string]string{}
		}
		opts.Metadata[key] = value
	}
}

// NewJSONMessage 将数据 JSON 序列化后封装为新消息，消息 ID 随机生成。
func NewJSONMessage(data any) *message.Message {
	bytes := json.MustMarshal(data)
	return message.NewMessage(uuid.NewString(), bytes)
}
