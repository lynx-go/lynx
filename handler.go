package lynx

import (
	"context"
	"errors"

	"github.com/lynx-go/lynx/eventbus"
)

// EventHandler 是业务侧对「一个类型化订阅」的完整声明：主题、handler 名、
// 依赖注入点与处理函数。业务结构体直接实现本接口，经 NewHandlerService
// 适配为 Service 后 app.Register——不需要嵌入骨架，也不需要构造后回填。
//
//	type OrderCreatedHandler struct {
//		name string
//		db   *sql.DB
//	}
//
//	func (h *OrderCreatedHandler) Topic() eventbus.Topic[OrderCreated] { return OrderCreatedTopic }
//	func (h *OrderCreatedHandler) HandlerName() string                { return h.name }
//	func (h *OrderCreatedHandler) Init(ctx lynx.AppContext) error     { return nil }
//	func (h *OrderCreatedHandler) Handle(ctx context.Context, e *eventbus.Event[OrderCreated]) error {
//		return h.db.Do(ctx, e.Payload)
//	}
//
//	app.Register(lynx.NewHandlerService(&OrderCreatedHandler{name: "order-created", db: db}))
type EventHandler[T any] interface {
	// Topic 返回订阅的类型化主题。
	Topic() eventbus.Topic[T]

	// HandlerName 是 Bus 内全局唯一的 handler 名，同时用作服务名
	//（启动/停止日志中的标识）。空名在 Init 期报错。
	HandlerName() string

	// Init 是依赖注入点：HandlerService 保证它在订阅之前调用，可用
	// AppContext 获取构造期不可得的依赖（配置、日志等）；无依赖时返回 nil。
	// 返回错误则不订阅，由 app.Run() 统一上抛。
	Init(ctx AppContext) error

	// Handle 处理一条已解码的事件。返回错误时按订阅选项重试或确认
	//（重试 / AutoAck / ContinueOnError 语义由 Bus 与 SubscribeOption 决定）。
	Handle(ctx context.Context, event *eventbus.Event[T]) error
}

// HandlerService 把 EventHandler 适配为 Service：
//
//   - Name：即 HandlerName，构造后立即可用（框架可能在 Init 前调用）；
//   - Init：先调用 h.Init 注入依赖，再订阅 Topic（依赖未就绪不会先消费）；
//   - Start：订阅已在 Init 完成，无自有循环，阻塞至关停（WaitForShutdown）；
//   - Stop：无资源可释放。
//
// 对比嵌入式骨架：适配器持有已构造好的 handler，所有方法静态可用，
// 不存在「基类回调派生类方法」的两阶段初始化与 nil 接口窗口。
type HandlerService[T any] struct {
	h    EventHandler[T]
	opts []eventbus.SubscribeOption
}

// NewHandlerService 创建 handler 服务。opts 透传给 Topic.Subscribe，
// 只影响该 handler 自身的投递语义（WithSubscribeRetry / WithAutoAck /
// WithContinueOnError）；默认以 HandlerName 作为 Bus 内 handler 名，
// 显式 eventbus.WithHandlerName 可覆盖。
//
// 同一事件的多个 handler 服务共享该事件的一条 transport 订阅并进程内
// 并行扇出：消费组 / 消费者成员数是后端配置（kafka consumer.group_id /
// instances），订阅级在途上限是事件配置（WithTopicMaxInFlight /
// Options.Topics.max_in_flight）。
func NewHandlerService[T any](h EventHandler[T], opts ...eventbus.SubscribeOption) *HandlerService[T] {
	return &HandlerService[T]{h: h, opts: opts}
}

// Name 返回服务名（即 handler 名）。不依赖 Init，注册前调用安全；
// 未配置（nil 服务 / nil handler）时返回空串而非 panic。
func (s *HandlerService[T]) Name() string {
	if s == nil || s.h == nil {
		return ""
	}
	return s.h.HandlerName()
}

// Init 注入依赖后订阅主题。顺序保证：h.Init 返回 nil 才会订阅。
func (s *HandlerService[T]) Init(ctx AppContext) error {
	if s == nil || s.h == nil {
		return errors.New("lynx: handler service is nil")
	}
	name := s.h.HandlerName()
	if name == "" {
		return errors.New("lynx: handler name must not be empty")
	}
	if err := s.h.Init(ctx); err != nil {
		return err
	}
	opts := make([]eventbus.SubscribeOption, 0, len(s.opts)+1)
	opts = append(opts, eventbus.WithHandlerName(name)) // 默认；调用方 opts 可覆盖
	opts = append(opts, s.opts...)
	return s.h.Topic().Subscribe(ctx.Context(), s.h.Handle, opts...)
}

// Start 订阅已在 Init 完成，无自有循环：阻塞至关停。
func (s *HandlerService[T]) Start(ctx context.Context) error { return WaitForShutdown(ctx) }

// Stop 无资源可释放。
func (s *HandlerService[T]) Stop(context.Context) error { return nil }

var _ Service = (*HandlerService[struct{}])(nil)
