# watermill

模块：`github.com/lynx-go/lynx/contrib/watermill`（独立 Go module）

Watermill 驱动的 `eventbus.Bus` 实现：Router 负责 handler 调度，收发经可插拔 `eventbus.Transport`（内存 / Kafka / 自定义）；`lynx.*` 生命周期事件强制走进程内内存 Transport，禁止出进程。

## 能力要点

- `New(opts eventbus.Options, ext ...Option) *Bus`（`bus.go:75`）：实现完整 `eventbus.Bus` 接口。Router 允许 0 handler 启动，`Start` 后仍可动态 `Subscribe`（`AddConsumerHandler` + `RunHandlers`，`bus.go:333`）
- `Route(topic, t)` / `RouteKey(topic, t, key)`（`bus.go:98` / `bus.go:104`）：绑定逻辑 topic 到 Transport；`key` 是 Transport 侧键，缺省 = 逻辑名
- `NewFromConfig(cfg lynx.Config, transports map[string]eventbus.Transport) (*Bus, error)`（`fromconfig.go:67`）：从 `bus:` 段装配路由与选项；标识 `"memory"` 的 Transport 兼作 `DefaultTransport`。不创建 Transport，由调用方创建并传入
- `NewMemoryTransport() *MemoryTransport`（`memory.go:26`）：gochannel 后端，广播语义，用于本地开发与默认回退
- 毒消息止损：`WithMaxRedeliveries(n)` / `WithTopicMaxRedeliveries(topic, n)`（`redelivery.go:38` / `redelivery.go:43`）；默认 `DefaultMaxRedeliveries = 10`（`redelivery.go:11`）
- Router 内置 `Recoverer` + `CorrelationID` 中间件（`bus.go:139`）；不装 `SignalsHandler`——信号只归 App
- `request_id` / `user_id` 日志属性发布时写入 Headers、消费时还原进 ctx（`bus.go:570`；白名单经 `eventbus.Options.PropagateAttrs` 配置）

## 快速开始

```go
package main

import (
	"context"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

var OrderCreated = eventbus.NewTopic[map[string]string]("order.created")

func main() {
	bus := watermill.New(eventbus.Options{
		DefaultTransport: watermill.NewMemoryTransport(), // 未显式 Route 的 topic 全部落内存
	})
	lynx.NewRunner(func(app lynx.App) error {
		return OrderCreated.Subscribe(app.Context(),
			func(ctx context.Context, e *eventbus.Event[map[string]string]) error {
				return nil // e.Payload 已反序列化为 map[string]string
			}, eventbus.WithHandlerName("audit"))
	},
		lynx.WithName("demo"),
		lynx.WithBus(bus), // Bus 的 Init/Start/Stop 由框架托管
	).Run()
}
```

## 配置（`bus:` 段）

`NewFromConfig` 经 `cfg.UnmarshalKey("bus", &file)` 读取（`fromconfig.go:69`），结构体定义见 `fromconfig.go:12`。

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `bus.debug` | bool | `false` | 放行底层 watermill 详细日志 |
| `bus.log_message.publish` | bool | `false` | 发布侧 Debug 日志 |
| `bus.log_message.subscribe` | bool | `false` | 消费侧 Debug 日志 |
| `bus.retry.max_retries` | int | `3` | handler 失败重试次数（`bus.go:594`） |
| `bus.retry.backoff` | duration | `0s` | 重试间隔；固定间隔，不是指数退避（`bus.go:604`） |
| `bus.max_redeliveries` | int | `10` | Bus 级毒消息重投上限；负数 = 不设限 |
| `bus.topics.<topic>.group` | string | 空 | 消费组（非内存 Transport 生效） |
| `bus.topics.<topic>.instances` | int | `0`（未设置） | 订阅实例数 |
| `bus.topics.<topic>.auto_ack` | bool | `false` | 先 Ack 后执行 handler（fire-and-forget） |
| `bus.topics.<topic>.continue_on_error` | bool | `false` | handler 失败仍 Ack，不重投 |
| `bus.topics.<topic>.retry` | 同 `bus.retry` | 沿用全局 | 主题级重试覆盖 |
| `bus.topics.<topic>.log_message` | 同上 | 沿用全局 | 主题级收发日志覆盖 |
| `bus.topics.<topic>.max_redeliveries` | int | `0`（沿用 Bus 级） | 主题级重投上限，优先于 Bus 级 |
| `bus.topics.<topic>.route.transport` | string | — | 引用 `NewFromConfig` 传入的 transports 标识；不存在则装配报错（`fromconfig.go:116`） |
| `bus.topics.<topic>.route.key` | string | 逻辑 topic 名 | Transport 侧键 |

`lynx.*` 主题禁止 route 到非内存 Transport：`RouteKey` 与 `Init` 双重校验，违规即报错（`bus.go:108`、`bus.go:149`）。

## 关键语义

- **`lynx.*` 强制内存 Transport**：`lynx.` 前缀的 topic 解析优先于路由表与 `DefaultTransport`，固定走 Bus 内置的 MemoryTransport（`bus.go:541`）。跨实例协同不应依赖生命周期事件。
- **消费组互斥**（`bus.go:447`）：非内存 Transport 上同一（订阅键 × 消费组）只允许一个 handler——Kafka 组内两个 handler 会瓜分分区、各收一半消息，第二个 `Subscribe` 直接拒绝。广播语义 = 每个 handler 不同 group（`eventbus.WithGroup` 或 `bus.topics.<topic>.group`）；竞争消费 = 单 handler + `eventbus.WithInstances`。已知边界：一个 handler 显式指定的 group 与另一个 handler 留空的 Transport 默认组同名时无法识别；物理 topics 重叠的不同逻辑 topic 需显式配置互不相同的组。
- **毒消息止损**（`redelivery.go:141`）：handler 终态失败（重试耗尽）后 Nack → Transport 重投（Kafka 默认约 100ms 一轮）。Bus 按 `handler|messageID` 计数累计重投轮数，超过 `max_redeliveries` 记 Error 并 Ack 丢弃，阻断无限重投与单分区队头阻塞。`auto_ack` 消息先 Ack 后执行，失败不重投、不计数。
- **Transport 生命周期独立于 Bus**：`Stop` 只关 Router 与内置生命周期 MemoryTransport，不关 `opts.Transports` / `DefaultTransport`（`bus.go:239`）——Kafka 等后端必须作为独立服务 Register 交框架托管，漏注册则永不关闭。
- **健康与就绪**：`CheckHealth` 仅反映 Router 运行标志，不做 broker 连通性检查（`bus.go:171`）；框架启动期按 `lynx.WithBusReadyTimeout`（默认 10s）有界轮询就绪，超时构造失败。
- **日志级别**：`log_message.*` 实际输出 Debug 级日志，需 `--log-level=debug` 或 `bus.debug: true` 才可见（`bus.go:322`）。

## 与 lynx 核心的集成

- `lynx.WithBus(bus)`：框架执行 `Init` → 提前 `Start` → 有界等待就绪 → 关停时 last-actor `Stop`；业务经 `app.Bus()` / `Topic[T]` 使用，无需感知 Router。
- `lynx.WithBusProvider(fn)`：总线依赖配置（`bus:` / `kafka:` 段）时，由框架在配置装配完成后调用 fn 构造；fn 可一并返回配套服务（如 Kafka Transport）交框架托管（kafka 版一行接入：`wmkafka.NewBusFromConfig`）。注入示例见 [watermill-kafka](../watermill-kafka/README.md)。
- 脱离框架单用：`New` → `Init(nil)` → `go Start(ctx)` → `Stop(ctx)`，见 `bus_test.go`。

## 相关文档

- [EventBus 设计](../../docs/design-eventbus.md)：wire 契约、启动/关停时序、决策记录
- [watermill-kafka](../watermill-kafka/README.md)：Kafka Transport（`kafka:` 段配置）
- [_examples/bus](../../_examples/bus)：默认内存 Bus 的同构用法
- [_examples/bus-kafka](../../_examples/bus-kafka)：watermill + kafka 跨进程示例（`WithBusProvider`）
- [eventbus 包](../../eventbus)：`Bus` / `Topic[T]` / `Event[T]` 接口与默认内存实现
