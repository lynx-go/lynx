package pubsub

import (
	"context"
)

// Handler 定义事件处理器的元信息与处理函数。框架按 handler 的声明解码
// 消息后调用 Handle：
//   - NewEvent 返回实现 MessageDecoder 的信封（如 *TypedMessage[T]）时，
//     框架经 topic 的 Marshaler 反序列化后调用 Handle；
//   - NewEvent 返回 nil 时按原始消息语义处理，Handle 收到 *Message；
//   - 否则按 NewEvent 的返回值原样调用 Handle（自定义解码路径）。
//
// 开发者通常无需手动实现本接口：使用 NewTypedHandler（类型化）或
// NewHandler（原始字节）工厂构造。
type Handler interface {
	EventName() string
	HandlerName() string
	NewEvent() any
	Handle(ctx context.Context, event any) error
}

// HandlerOptions 可为 Handler 附加订阅选项。
type HandlerOptions interface {
	Options() []SubscribeOption
}

// NewTypedHandler 创建类型化事件处理器：消息 Payload 经 topic 的 Marshaler
// 反序列化到 T 后调用 handle。元数据（ID/Key/Headers）随 TypedMessage
// 信封传入，无需额外上下文查询。
func NewTypedHandler[T any](topic, handlerName string, handle func(ctx context.Context, event *TypedMessage[T]) error, opts ...SubscribeOption) Handler {
	return &typedHandler[T]{
		topic: topic, handlerName: handlerName,
		handle: handle, opts: opts,
	}
}

// NewHandler 创建原始字节事件处理器：Handle 直接收到 *Message。
func NewHandler(topic, handlerName string, handle func(ctx context.Context, event *Message) error, opts ...SubscribeOption) Handler {
	return &rawHandler{
		topic: topic, handlerName: handlerName,
		handle: handle, opts: opts,
	}
}

var _ Handler = (*typedHandler[struct{}])(nil)
var _ Handler = (*rawHandler)(nil)

// typedHandler 是 NewTypedHandler[T] 的擦除实现：类型参数 T 仅存在于
// 构造期，实现非泛型 Handler 接口。
type typedHandler[T any] struct {
	topic       string
	handlerName string
	handle      func(ctx context.Context, event *TypedMessage[T]) error
	opts        []SubscribeOption
}

func (h *typedHandler[T]) EventName() string   { return h.topic }
func (h *typedHandler[T]) HandlerName() string { return h.handlerName }
func (h *typedHandler[T]) NewEvent() any       { return &TypedMessage[T]{} }
func (h *typedHandler[T]) Options() []SubscribeOption {
	return h.opts
}

func (h *typedHandler[T]) Handle(ctx context.Context, event any) error {
	return h.handle(ctx, event.(*TypedMessage[T]))
}

// rawHandler 是 NewHandler 的实现：NewEvent 返回 nil 表示原始语义，
// Handle 收到 *Message。
type rawHandler struct {
	topic       string
	handlerName string
	handle      func(ctx context.Context, event *Message) error
	opts        []SubscribeOption
}

func (h *rawHandler) EventName() string   { return h.topic }
func (h *rawHandler) HandlerName() string { return h.handlerName }
func (h *rawHandler) NewEvent() any       { return nil }
func (h *rawHandler) Options() []SubscribeOption {
	return h.opts
}

func (h *rawHandler) Handle(ctx context.Context, event any) error {
	return h.handle(ctx, event.(*Message))
}
