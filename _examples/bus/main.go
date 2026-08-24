package main

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
)

// OrderCreated 是示例域事件。
type OrderCreated struct {
	OrderID string `json:"order_id"`
	UserID  string `json:"user_id"`
}

// 定义类型化主题：编译期即绑定 Payload 类型与默认选项。
var OrderCreatedTopic = eventbus.NewTopic[OrderCreated]("order.created")

// orderService 发布订单事件。
type orderService struct{}

func (s *orderService) Name() string { return "order-service" }
func (s *orderService) Init(ctx lynx.AppContext) error {
	slog.Info("order-service init, bus available", "bus", ctx.Bus().Name())
	return nil
}
func (s *orderService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (s *orderService) Stop(ctx context.Context) error { return nil }

// auditService 订阅订单事件，实现组件间协同。
type auditService struct{}

func (s *auditService) Name() string { return "audit-service" }
func (s *auditService) Init(ctx lynx.AppContext) error {
	// Topic.Subscribe：Bus 从 Context / Default 解析，Payload 自动反序列化
	return OrderCreatedTopic.Subscribe(ctx.Context(), "audit-handler",
		func(ctx context.Context, e *eventbus.Event[OrderCreated]) error {
			slog.InfoContext(ctx, "audit received order", "order_id", e.Payload.OrderID, "user_id", e.Payload.UserID)
			return nil
		})
}
func (s *auditService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *auditService) Stop(ctx context.Context) error  { return nil }

// inventoryService 演示原始事件订阅。
type inventoryService struct{}

func (s *inventoryService) Name() string { return "inventory" }
func (s *inventoryService) Init(ctx lynx.AppContext) error {
	return ctx.Bus().Subscribe(ctx.Context(), "order.created", "inventory-handler",
		func(ctx context.Context, e *eventbus.RawEvent) error {
			slog.InfoContext(ctx, "inventory received", "payload", string(e.Payload))
			return nil
		})
}
func (s *inventoryService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *inventoryService) Stop(ctx context.Context) error  { return nil }

// lifecycleCoordinator 演示通过内建生命周期事件实现组件间协同：
// 订阅 App/Service/HTTP 事件，其他组件可据此决定何时开始工作。
type lifecycleCoordinator struct{}

func (s *lifecycleCoordinator) Name() string { return "lifecycle-coordinator" }
func (s *lifecycleCoordinator) Init(ctx lynx.AppContext) error {
	// 订阅 App 级事件（Topic 方法，无需手传 Bus）
	_ = eventbus.AppStartedTopic.Subscribe(ctx.Context(), "coord-app-started", func(ctx context.Context, e *eventbus.Event[eventbus.AppEvent]) error {
		slog.InfoContext(ctx, "coordinator: app started", "name", e.Payload.Name, "id", e.Payload.ID)
		return nil
	})
	_ = eventbus.ServiceRegisteredTopic.Subscribe(ctx.Context(), "coord-service-registered", func(ctx context.Context, e *eventbus.Event[eventbus.ServiceEvent]) error {
		slog.InfoContext(ctx, "coordinator: service registered", "service", e.Payload.Service)
		return nil
	})
	_ = eventbus.ServiceStartedTopic.Subscribe(ctx.Context(), "coord-service-started", func(ctx context.Context, e *eventbus.Event[eventbus.ServiceEvent]) error {
		slog.InfoContext(ctx, "coordinator: service started", "service", e.Payload.Service)
		return nil
	})
	// 若有 HTTP 服务，可订阅其 listening 事件以获知实际监听地址
	_ = eventbus.HTTPListeningTopic.Subscribe(ctx.Context(), "coord-http-listening", func(ctx context.Context, e *eventbus.Event[eventbus.ServerEvent]) error {
		slog.InfoContext(ctx, "coordinator: http listening", "addr", e.Payload.Addr, "advertise", e.Payload.AdvertiseAddr)
		return nil
	})
	return nil
}
func (s *lifecycleCoordinator) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *lifecycleCoordinator) Stop(ctx context.Context) error  { return nil }

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {
		// Bus 开箱即用，无需 Register；直接通过 Topic / app.Bus() 发布/订阅
		// lifecycleCoordinator 需最先注册，以便捕获后续服务的 Registered/Started 事件
		app.Register(&lifecycleCoordinator{}, &orderService{}, &auditService{}, &inventoryService{})

		// 演示：OnStart 中发布事件，所有订阅者（同进程）即时收到
		app.OnStart(func(ctx context.Context) error {
			// 方式1：类型化发布（Topic.Publish）
			_ = OrderCreatedTopic.Publish(ctx, OrderCreated{OrderID: "123", UserID: "u1"})
			// 方式2：原始/字符串 topic 发布
			_ = app.Bus().Publish(ctx, "order.created", map[string]string{"order_id": "456"})
			return nil
		})
		return nil
	}, lynx.WithName("bus-example"))
	runner.Run()
}
