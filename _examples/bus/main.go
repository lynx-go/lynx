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
	// 一行订阅：Topic 携带类型，Payload 自动反序列化
	return eventbus.SubscribeTyped(ctx.Context(), ctx.Bus(), OrderCreatedTopic, "audit-handler",
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

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {
		// Bus 开箱即用，无需 Register；直接通过 app.Bus() 发布/订阅
		app.Register(&orderService{}, &auditService{}, &inventoryService{})

		// 演示：OnStart 中发布事件，所有订阅者（同进程）即时收到
		app.OnStart(func(ctx context.Context) error {
			// 方式1：类型化发布
			_ = eventbus.PublishTyped(ctx, app.Bus(), OrderCreatedTopic, OrderCreated{OrderID: "123", UserID: "u1"})
			// 方式2：原始发布
			_ = app.Bus().Publish(ctx, "order.created", map[string]string{"order_id": "456"})
			return nil
		})
		return nil
	}, lynx.WithName("bus-example"))
	runner.Run()
}
