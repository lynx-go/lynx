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
| `bus.topics.<topic>.max_in_flight` | int | `1` | 订阅级在途上限：同一事件未确认消息的并发上限（消息内多 handler 仍并行）；`1` = 串行且保序，负数 = 不限制（不推荐） |
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
- **事件订阅复用**（`subscription.go`）：订阅单元是事件（逻辑 topic）——同一事件的多个 handler 共享一条 transport 订阅并进程内并行扇出，每个 handler 都收到每条消息。消费组 / 消费者成员数是后端配置（kafka `consumer.group_id` / `consumer.instances`），Bus 不建模。已知边界：物理 topics 重叠的不同逻辑 topic 需拆成不同 kafka 条目。
- **订阅级在途上限**（`bus.topics.<topic>.max_in_flight`，默认 1）：适配器在把消息交给 Router 之前占用槽位、Ack/Nack/订阅关停时释放——在途消息有界（goroutine 上界 ≈ handler 数 + 2）并对 transport 形成背压；默认串行且恢复投递顺序，调大即并发处理，负数 = 不限制（不推荐）。只限 dispatcher 执行挡不住 Router 的每消息 goroutine 堆积，限流点必须在适配器。**Kafka 提交顺序注意**：watermill-kafka 在 Ack 时 `MarkMessage(offset+1)` 并提交，提交高 offset 隐含提交更低 offset——`max_in_flight>1` 时乱序确认会打开「崩溃跳过仍在处理的低 offset 消息」的窗口（at-least-once 弱化）；要严格 at-least-once 保持默认 1。
- **毒消息止损**（`redelivery.go` / `subscription.go`）：handler 终态失败（重试耗尽）后 Nack → Transport 重投（Kafka 默认约 100ms 一轮）。Bus 按 `handler|messageID` 计数累计终态失败轮数；共享 offset 下全部 handler 成功才 Ack，某 handler 超过 `max_redeliveries` 后被跳过并记 Error（不连坐其他 handler）；重投会整条重投，成功过的 handler 也需幂等。`auto_ack` handler 不参与确认裁决。
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
