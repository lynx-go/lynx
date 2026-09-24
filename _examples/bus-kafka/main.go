// bus-kafka 示例：watermill + kafka 跨进程 EventBus。
//
// 演示 v1.12.0 的 lynx.WithBusProvider：总线依赖配置（bus:/kafka: 段），
// 由框架在配置装配完成后调用 provider 构造。kafka 版装配入口
// wmkafka.NewBusFromConfig 直接匹配 provider 签名——kafka Transport 作为
// 配套服务返回，生命周期（Start/Stop/健康聚合）由框架托管，无需在
// NewRunner 之前自行读配置再 WithBus 注入；装配细节见 watermill-kafka
// README（含自定义 transport 的手工路径）。
//
// 需要本地 kafka：启动方式见 README.md。双终端各跑一个实例可观察
// 消费组语义（同 group 竞争消费，每条消息只投递给其中一个）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"
	"github.com/lynx-go/lynx/eventbus"
)

// OrderCreated 是示例域事件。
type OrderCreated struct {
	OrderID string `json:"order_id"`
	UserID  string `json:"user_id"`
}

// OrderCreatedTopic 类型化主题：经 config.yaml 的
// bus.topics.order.created.route 路由到 kafka transport（跨进程投递），
// Payload 自动 JSON 序列化。
var OrderCreatedTopic = eventbus.NewTopic[OrderCreated]("order.created")

// orderService 周期发布订单事件：WithMessageKey 决定 Kafka 分区键
// （record Key），同键保证有序。
type orderService struct{}

func (s *orderService) Name() string { return "order-service" }
func (s *orderService) Init(ctx lynx.AppContext) error {
	slog.Info("order-service init, bus available", "bus", ctx.Bus().Name())
	return nil
}

func (s *orderService) Start(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			id := fmt.Sprintf("%d", n)
			if err := OrderCreatedTopic.Publish(ctx, OrderCreated{OrderID: id, UserID: "u1"},
				eventbus.WithMessageKey(id)); err != nil {
				slog.ErrorContext(ctx, "publish failed", "error", err)
			}
		}
	}
}

func (s *orderService) Stop(context.Context) error { return nil }

// auditService 订阅订单事件：kafka 消费组由 config.yaml 的
// kafka.order.created.consumer 配置（group_id/instances）； lynx.* 生命周期
// 事件强制走内存 transport（route 到 kafka 会在 Init 期报错）。
type auditService struct{}

func (s *auditService) Name() string { return "audit-service" }
func (s *auditService) Init(ctx lynx.AppContext) error {
	return OrderCreatedTopic.Subscribe(ctx.Context(),
		func(ctx context.Context, e *eventbus.Event[OrderCreated]) error {
			slog.InfoContext(ctx, "audit received order",
				"order_id", e.Payload.OrderID, "user_id", e.Payload.UserID, "key", e.Key)
			return nil
		}, eventbus.WithHandlerName("audit-handler"))
}
func (s *auditService) Start(ctx context.Context) error { return lynx.WaitForShutdown(ctx) }
func (s *auditService) Stop(context.Context) error      { return nil }

func main() {
	lynx.NewRunner(func(app lynx.App) error {
		app.Register(
			&orderService{},
			&auditService{},
			// handler 服务：同一事件（order.created）的多个 handler 共享
			// 该事件的一条 Kafka 订阅（消费组取自配置）并进程内并行扇出，
			// 每个 handler 都收到每条消息。
			lynx.NewHandlerService(&OrderCreatedHandler{name: "OrderCreatedHandler"}),
			lynx.NewHandlerService(&OrderCreatedHandler{name: "OrderCreatedHandler2"}),
		)
		return nil
	},
		lynx.WithName("bus-kafka-example"),
		lynx.WithBusProvider(wmkafka.NewBusFromConfig),
	).Run()
}
