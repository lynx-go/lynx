# Lynx EventBus 设计方案

| 字段 | 内容 |
| --- | --- |
| 标题 | Lynx EventBus（一等消息总线） |
| 作者 | TBD |
| 日期 | 2026-08-24 |
| 状态 | **Implemented**（已合入 `master`；实现见 `eventbus/`、`contrib/watermill`、`contrib/watermill-kafka`） |
| 适用版本 | v1.1+ |
| 相关讨论 | 消息路径审查、移除 `contrib/pubsub`、生命周期走 Watermill 但锁死内存 Transport（方案 B）、gocloud vs Watermill、Bus 先于 Component 启动、关停时序、Transport `Delivery` Ack/Nack |

---

## 1. Overview

Lynx 将 **EventBus** 提升为一等子系统：进程内组件协同与跨进程领域事件共用同一套开发者接口（`Bus` / `Topic[T]` / `Event[T]`），默认内存零配置可用；跨进程后端通过可插拔 `Transport` 扩展，首个生产实现基于 Watermill 的 Publisher/Subscriber（Kafka 等），**不以 Watermill Router / gocloud 作为总线内核语义来源**。

本方案同时收敛既有双栈（`eventbus` + `contrib/pubsub`），明确 wire 契约、**生命周期与领域事件共用同一 `Bus` 实现路径（方案 B）但 `lynx.*` 强制进程内内存 Transport**、启动/关停时序，以及删除旧模块时的能力迁移边界。

### 1.1 设计原则（验收标准）

| 原则 | 含义 |
| --- | --- |
| 有效 DX | 业务侧以 `Topic[T]` 为锚点发布/订阅；类型在编译期对齐 |
| 低心智负担 | 一个 `app.Bus()`；不必区分「内存协同总线」与「领域总线」的公开 API |
| 默认开箱可用 | `Options.Bus == nil` → 内存 Bus；无 Kafka/配置也能跑通组件协同 |
| 配置与扩展开放 | `Options.Topics` / Marshaler / Transport / 可选 `RouteKey` 与配置装配在 contrib |

### 1.2 非目标

- 不把 EventBus 做成通用消息框架（不重写 Kafka/NATS 客户端）。
- 不以 gocloud pubsub 为底座（见 §6）。
- **禁止**跨进程投递 `lynx.*` 生命周期事件（服务发现另见 registry 设计）；实现上强制内存 Transport，配置指向外部 Broker 则 Init 失败。
- RocketMQ 等仅为扩展举例，本阶段不交付。

---

## 2. Background & Motivation

### 2.1 背景（设计时快照，已落地）

设计启动时：核心已有 `eventbus` 与默认 `NewMemoryBus`；`contrib/pubsub` + `contrib/kafka` 为旧双栈。**合入后**：仅保留 `eventbus` + `contrib/watermill` + `contrib/watermill-kafka`；见附录 A。

历史问题摘要（均已收敛）：
- 出进程时信封字段（`Time`/`Topic`）易丢；`Key` 与 Kafka 分区键语义错接。
- Watermill `Subscribe` 在 started 后拒绝注册；关停 `subscriberAdapter.Close` 空实现。
- `Transport.Subscribe` 丢 Ack 句柄会导致 Kafka 消费停摆（已改为 `Delivery`）。

### 2.2 要解决的问题

1. **消息路径**：内存与跨进程路径上，类型转换不出现错接、断链、静默丢字段。
2. **双栈收敛**：删除 `contrib/pubsub`，Kafka 接到 `eventbus.Transport`，保留现有生产能力（fan-in、instances、SASL/TLS 等）。
3. **生命周期（方案 B）**：`lynx.*` 与领域事件走 **同一 `app.Bus()` / 同一 Watermill（或内存）Bus 实现**，但路由 **锁死进程内内存 Transport**，永不进 Kafka；配置违规则 Init 报错。
4. **Init 可用完整 Bus**：Component `Init` 中可 Subscribe + Publish；Watermill 侧利用官方「Run 后 `AddConsumerHandler` + `RunHandlers`」（含生命周期订阅）。
5. **底座选型**：明确自研 API + Watermill 作驱动箱，而非 gocloud 或从零造 Broker 客户端。

---

## 3. Architecture

### 3.1 逻辑视图（方案 B）

```
开发者
  └─ app.Bus()  →  唯一 eventbus.Bus 实例（Options.Bus / WithBus）
                      ├─ 默认：eventbus.NewMemoryBus（全部 topic，含 lynx.*）
                      └─ 扩展：contrib/watermill.Bus
                                 ├─ lynx.*  →  强制 MemoryTransport（进程内）
                                 │              禁止 Route 到 kafka 等；Init 校验失败则报错
                                 └─ 领域 topic →  可配 kafka / memory / …
```

要点：

- **一条 Bus、一套 Subscribe/Publish**，生命周期也走 Watermill Router（当用户注入 Watermill Bus 时），不再做 App 层「双 Bus 门面分流」。
- **进程内硬约束**靠路由规则落实：`lynx.*` 解析结果必须是内存 Transport；不是靠第二套旁路 Bus。
- 默认 `NewMemoryBus` 时无 Watermill，生命周期与领域事件本就同进程，无需特殊分支。

对外三个概念：**Bus / Topic[T] / Event[T]**。`Transport`、RouteKey、Router（若保留薄装配）、Watermill `message.Message` 均不下沉为公共业务 API。

### 3.2 模块落点

| 模块 | 职责 |
| --- | --- |
| `eventbus/`（核心） | `Bus` 接口、`Topic[T]`、`Event`/`RawEvent`、Marshaler、`DecodeTyped`、内存 Bus、**wire 信封映射**、生命周期 Topic 常量与类型；可选：`IsLifecycleTopic(name)` 供路由校验 |
| `lynx` 核心 | `App.Bus()`、`WithBus`、Bus 先于组件的 Init/Start、关停 last-actor；`publishEvent` 直接打 `app.bus` |
| `contrib/watermill` | `eventbus.Bus` 实现：Router + 动态 `RunHandlers`；**Init 时为 `lynx.*` 绑定/注入 MemoryTransport**，并拒绝外部路由；可含通用 MemoryTransport |
| `contrib/watermill-kafka` | Kafka 的 `eventbus.Transport`（由现 `contrib/kafka` **重命名**并改缝，能力不缩水） |
| 删除 | `contrib/pubsub` 整模块；示例/文档迁到 Bus 路径；`contrib/watermill` 内瘦身 `kafka.go` 并入或删除，避免双份 Kafka |

### 3.3 两类流量（同一 Bus，不同 Transport 约束）

| 流量 | Topic | Transport 约束 | 语义 |
| --- | --- | --- | --- |
| 生命周期 / 组件协同 | `lynx.*` | **仅**进程内内存（MemoryBus 本体或 Watermill 的 MemoryTransport） | 同进程、Init/Register 当下可订可收；**禁止**出进程 |
| 领域事件 | 业务名（如 `order.created`） | 可配 kafka / memory / … | 可跨进程；at-least-once（依赖 Transport） |

路由规则（Watermill Bus）：

1. 逻辑名以 `lynx.` 为前缀 → 固定走内置 MemoryTransport（可在 Init 自动注册，无需用户配置）。
2. 用户 `Route` / 配置 `topics.*.route` 若将 `lynx.*` 指向非内存 Transport → **Init 报错**，禁止静默。
3. `DefaultTransport` 为 Kafka 时，**不得**回落承接 `lynx.*`（前缀规则优先于 DefaultTransport）。

---

## 4. Public Interface

### 4.1 Bus（实现面 / 逃生仍经 Option，不另开 Typed 重载）

```go
type Bus interface {
    Publish(ctx context.Context, topic string, payload any, opts ...PublishOption) error
    PublishRaw(ctx context.Context, topic string, data []byte, opts ...PublishOption) error
    Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
    MarshalerFor(topic string) Marshaler

    Name() string
    Init(ctx InitContext) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    CheckHealth() error
}
```

说明：

- **业务主路径**是 `Topic[T]` 的方法（§4.2），不直接调用上述字符串 API。
- `Route` / `RouteKey` **不进入核心 Bus 接口**；由 Watermill Bus 构造选项或 `NewFromConfig`（contrib）提供。
- Bus 健康可不进入 readiness 聚合（与现状 `busService` 一致）。
- `AppContext.Bus()` / `lynx.WithBus`（应用构造）仍保留，用于装配与显式取实例；**日常 Publish/Subscribe 不要求调用方手传 Bus**。

### 4.2 Topic[T] 主 API 与 Bus 解析

```go
type Topic[T any] struct { /* name + 默认订阅/发布选项 */ }

func (t Topic[T]) Publish(ctx context.Context, payload T, opts ...PublishOption) error
func (t Topic[T]) Subscribe(ctx context.Context, handlerName string, h func(context.Context, *Event[T]) error, opts ...SubscribeOption) error
// PublishRaw / 转发等同理，均不把 Bus 作为位置参数
```

**Bus 解析顺序（已拍板）**：

1. **`eventbus.WithBus(b)`**（`PublishOption` / `SubscribeOption`）— 测试、多总线、显式覆盖；**不**再提供 `Publish(ctx, bus, …)` 第二套签名  
2. **`BusFromContext(ctx)`** — `newLynx` 把 Bus 写入 `app.Context()`；入站中间件向请求 ctx 注入  
3. **`eventbus.Default()`** — `newLynx` 调用 `SetDefault(bus)`（类比 `slog.SetDefault`）；皆无则返回明确错误  

```go
// 框架侧（示意）
eventbus.SetDefault(bus)
app.ctx = eventbus.ContextWithBus(app.ctx, bus)

// 业务日常
OrderCreated.Publish(ctx, order)
OrderCreated.Subscribe(app.Context(), "audit", handler)

// 逃生口：同一方法 + Option，不单列接口
OrderCreated.Publish(ctx, order, eventbus.WithBus(other))
OrderCreated.Subscribe(ctx, "audit", handler, eventbus.WithBus(other))
```

注意命名空间：

| 符号 | 包 | 含义 |
| --- | --- | --- |
| `lynx.WithBus` | `lynx` | 应用构造 Option，注入整应用的 Bus |
| `eventbus.WithBus` | `eventbus` | 单次 Publish/Subscribe 的 Option，覆盖解析结果 |

包级 `PublishTyped` / `SubscribeTyped`：**改为** `Topic` 方法的薄别名或迁移期后删除，避免双入口。

`NewTopic` 选项（`WithTopicMarshaler` / Group / Instances / AutoAck / ContinueOnError / Retry）须在 Subscribe 路径 **实际生效**（见 §5.2）。

```go
type Event[T any] struct {
    ID, Topic, Key string
    Headers        map[string]string
    Payload        T
    Time           time.Time
}
type RawEvent struct { /* 同上，Payload []byte */ }
```

### 4.3 Transport（扩展缝）

```go
type Delivery struct {
    Event *RawEvent
    Ack   func()
    Nack  func()
}

type Transport interface {
    Publish(ctx context.Context, topic string, e *RawEvent) error
    Subscribe(ctx context.Context, topic string, opts SubscribeOptions) (<-chan Delivery, error)
    Topics() []string
    Close() error
}
```

- `topic` 一律为 **Transport 侧键**（经 Bus 路由解析后的 key；缺省等于逻辑名）。
- `Delivery` 把**信封**与**确认句柄**分开：`RawEvent` 保持纯数据；Ack/Nack 由 Bus 在 handler 成功/失败（及 AutoAck）时调用，转达到底层 broker（Kafka offset、gochannel 等）。
- 公共业务路径 **不出现** `*message.Message`，也**不出现** `Delivery`（仅扩展缝与 Bus 实现使用）。
- Kafka 的物理 topic、消费组、instances、认证等留在 `contrib/watermill-kafka` 配置内。

### 4.4 开发者最小用法

```go
var OrderCreated = eventbus.NewTopic[Order]("order.created")

func (s *Audit) Init(ctx lynx.AppContext) error {
    return OrderCreated.Subscribe(ctx.Context(), "audit",
        func(ctx context.Context, e *eventbus.Event[Order]) error { /* ... */ return nil })
}

// 发布（ctx 带 Bus 或回退 Default；属性仍从 ctx 传播）
_ = OrderCreated.Publish(ctx, Order{ID: "1"})

// 生命周期
_ = eventbus.AppStartedTopic.Subscribe(ctx.Context(), "coord",
    func(ctx context.Context, e *eventbus.Event[eventbus.AppEvent]) error { /* ... */ return nil })
```

---

## 5. Wire 契约与类型转换

### 5.1 单一映射点

`RawEvent` ↔ 底层消息（Watermill `message.Message` 或等价）的转换 **只实现一次**，放在 `eventbus`（或 watermill 包内唯一调用的 `eventbus` helper），禁止 Transport 与 Bus 各写一份不一致逻辑。

| 字段 | Wire | 消费还原 |
| --- | --- | --- |
| Payload | 消息体 | 原样 |
| ID | UUID / 消息 ID | 原样 |
| Key | metadata `x-message-key`；**且**若后端支持分区键则同时写入 record key（见下） | `Event.Key`；不残留在 Headers |
| Headers | metadata（去掉协议键） | 原样 |
| Topic | 可选 metadata `x-logical-topic`；物理名由 Transport 决定 | handler 侧逻辑名以订阅注册为准，可与 metadata 校验 |
| Time | metadata `x-event-time`（RFC3339Nano） | 还原；缺失时可用 `time.Now()` 并文档标明降级 |

### 5.2 Marshaler 优先级（发布/订阅对称）

统一规则（高 → 低）：

1. 本次 `PublishOption` / `SubscribeOption` 显式 Marshaler  
2. `Topic[T]` 携带的 Marshaler  
3. `Options.TopicMarshalers[topic]`  
4. `Options.Marshaler`  
5. `JSONMarshaler`

要求：

- `Subscribe` 实现 **必须读取** `SubscribeOptions.Marshaler`（或解码闭包与 Publish 使用同一解析函数）。
- 禁止「`PublishTyped` 用 Topic Marshaler、同名字符串 `Publish` 用全局 JSON」的静默错接；Debug 下应对齐告警（可选：同 topic 首次不一致时 Error 日志）。
- `T == []byte`：Payload 透传，不经 Marshaler（与现状一致）。

### 5.3 Key 与 Kafka 分区键

- **业务语义**：`WithMessageKey` / `Event.Key` 表示业务消息键。
- **Kafka**：适配器必须把该键写入 **Kafka record key**（分区/保序），不能仅放 header。实现可选：自定义 Watermill Kafka Marshaler，或在 Transport.Publish 组装 ProducerMessage。
- `maps.Copy(Headers)` **不得覆盖** 已写入的 `x-message-key`（先 Copy 再 Set key，或 Copy 时跳过协议键）。

### 5.4 PublishRawTyped / 转发

转发应保留 `ID`、`Key`、`Headers`、`Time`、`Payload`；仅当明确「新事件」语义时才生成新 ID/Time。文档与 API 命名区分 **转发** vs **新发**。

### 5.5 内存路径特殊语义

- 缓冲满：**丢弃并打 Error 日志**（状态协同不反压发布者）。文档写明：内存 Bus ≠ 可靠队列。
- `Publish(*RawEvent)`：参数 `topic` 与 `RawEvent.Topic` 冲突时，**以函数参数 topic 为准**（与 Watermill 路径一致），避免静默改道。

---

## 6. 底座选型结论

| 候选 | 结论 |
| --- | --- |
| **自研 `eventbus` API** | **采用**。深模块：类型化 Topic、生命周期、App 集成。 |
| **Watermill Pub/Sub** | **采用为驱动箱**。Kafka / 未来 NATS JetStream、Redis Streams 等有现成适配；Router 仅作 handler 调度，**不是**业务 API。 |
| **gocloud pubsub** | **不采用为底座**。`Receive` 拉模型与 Service 生命周期不合；无一等 Key；NATS 驱动 gob 编码破坏跨语言；主仓基本不收新驱动；与 Lynx 配置驱动（按逻辑 topic 表）摩擦大。若未来要接 GCP/SQS，可作为 **某一个 Transport**，而非整座内核。 |
| **从零写 Broker 客户端** | **禁止**（Kafka/NATS/Redis）。只写薄 `Transport`。 |

---

## 7. 生命周期与启动时序

### 7.1 方案 B：同一 Bus，`lynx.*` 锁死内存 Transport

**已拍板**：生命周期事件走与领域事件相同的 `app.Bus()` 实现路径（含 Watermill Router / 动态 handler），**不**在 App 层维护第二套旁路 Bus。

约束：

- `lynx.*` **仅**进程内有效；跨实例协同用 registry，禁止把 `lynx.http.listening` 等配进 Kafka。
- Watermill Bus 在 `Init` 中：
  - 确保存在专用于生命周期的 MemoryTransport（或等价 gochannel）；
  - 将所有 `lynx.*`（含未来新增常量）解析到该 Transport；
  - 校验用户路由/配置：凡 `lynx.` 前缀指向非内存 → 返回错误。
- 默认 `NewMemoryBus`：无额外逻辑，全部 topic 本就内存。
- Marshaler：生命周期 payload 使用 Bus 的 `MarshalerFor(topic)`（默认 JSON）；不单独强制另一套，避免「同一 Bus 两套默认」；业务不应为 `lynx.*` 注册自定义 `TopicMarshalers`（文档约定；可选 Init 告警）。

与方案 A（旁路双 Bus）的差异：开发者心智是真·单一 Bus；进程内保证靠 **路由锁** 而非 **门面分流**。

### 7.2 Bus 先于 Component（目标时序）

```
newLynx
  → Bus.Init（Watermill：装好 lynx.* → MemoryTransport 路由并校验配置）
  → Bus.Start（后台；Watermill：空 Router 可 Run，等 handlerAdded）
  → 等待 Running / CheckHealth（有界）
Register / Service.Init
  → Subscribe（含 lynx.*）→ AddConsumerHandler + RunHandlers
  → publishEvent / Publish 可被已订阅者收到
Run
  → OnStart hooks
  → 其余 Service Start（HTTP/gRPC/…）
  → …运行…
```

要点：

- Watermill 官方支持 Run 后 `AddConsumerHandler` + `RunHandlers`；删除 `started` 后拒绝 Subscribe 的门闩。
- 去掉 `plugin.SignalsHandler`，信号只归 App。
- 生命周期与领域 handler 共用同一 Router / handlerName 唯一空间（见 O1 已决：共用即可，因只有一个 Bus）。
- Kafka consumer 未就绪只影响领域 topic，**不影响** `lynx.*`（内存 Transport）。

### 7.3 Init 阶段「完整 Bus」的含义

| 能力 | Init 时保证 |
| --- | --- |
| Subscribe / SubscribeTyped（含 `lynx.*`） | 是 |
| Publish 到已订阅的同进程 handler | 是 |
| 跨进程消费者已 rebalance | 否（仅领域；健康检查表达） |
| `lynx.service.registered` 等 | 是（经同一 Bus → 内存 Transport） |

---

## 8. 关停时序

保持 **Bus last-actor**，与「Bus 提前 Start」正交。

```
触发：信号 / Close / 某 Service Start 失败
  1. lynx.app.stopping
  2. Drain（可选）：readiness 失败 ∥ OnDrain；drain 事件经同一 Bus（lynx.* → 内存）发出
  3. cancel app.ctx
  4. OnStop hooks（服务仍在跑；预算 ShutdownTimeout）
  5. run.Group 逆序 interrupt：各 Service Stopping → Stop → Stopped
  6. lynx.app.stopped          ← Bus 仍可用
  7. Bus Stopping → Bus.Stop → Bus Stopped
```

约束：

- 组件 `Stop` **不得**单独关闭整条共享 Bus / Router。
- 注意 Watermill：最后一个 handler 停止可能触发 Router 自 Close；关停应由 App 统一 `Bus.Stop`，避免收尾事件窗口被提前掐断。
- `Bus.Stop` 继续走有界 `StopTimeout`。

时长上界：`max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout + Σ StopTimeout + Bus.Stop`。

---

## 9. 删除 contrib/pubsub 与 Kafka 迁移

### 9.1 必须迁走的能力

| 旧能力 | 迁至 |
| --- | --- |
| Broker Publish/Subscribe + Marshaler | `eventbus.Bus` + Typed helpers |
| `Route` / `RouteKey` + `NewFromConfig` | `contrib/watermill` 构造 / 配置装配（不进核心接口） |
| Kafka Transport（fan-in、instances、SASL/TLS、auto-commit…） | **`contrib/watermill-kafka`**（现 `contrib/kafka` 重命名）改实现 `eventbus.Transport` |
| `MessageID`/`MessageKey` 入 ctx | Watermill wrapHandler 恢复（或文档弃用并给替代） |
| 示例 `_examples/pubsub`、cli 中的 Broker | `_examples/bus` 路径 |

### 9.2 刻意不迁（降低心智）

- `Handler` / `NewEvent() any` 类型擦除 → 用 `Topic[T]` + `SubscribeTyped`
- 公共 API 暴露 `*message.Message`
- 与 `eventbus.Options` 重复的第二套 Options（配置键从 `pubsub.*` 迁到 `bus.*` 或沿用 `Topics`）

### 9.3 禁止用瘦实现顶替

`contrib/watermill/kafka.go` 当前最小实现（单物理 topic、忽略 Instances 等）**不得**作为生产 Kafka。必须以现 `contrib/kafka/transport.go` 能力为基线，迁入 **`contrib/watermill-kafka`** 后改缝；随后删除 watermill 包内瘦 `kafka.go`，只保留一处 Kafka Transport。

### 9.4 发布与模块

- `go.work` / CI / Taskfile / RELEASE / README / CLAUDE.md / docs 去掉 pubsub，并将 kafka 模块路径改为 watermill-kafka。
- 模块版本策略：在 CHANGELOG 标明 breaking：移除 `contrib/pubsub`；`contrib/kafka` → `contrib/watermill-kafka`；依赖 `eventbus.Transport`。

### 9.5 模块重命名：`contrib/kafka` → `contrib/watermill-kafka`

**已拍板**：目录与 Go module 路径重命名，表明「Watermill 生态下的 Kafka Transport」，与 `contrib/watermill`（Bus）成对出现。

| 项 | 值 |
| --- | --- |
| 目录 | `contrib/watermill-kafka/` |
| Module | `github.com/lynx-go/lynx/contrib/watermill-kafka` |
| 导入示例 | `import wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"` |
| 配置段 | 仍建议 `kafka:`（业务配置名不变；包名与配置键可分离） |
| 发版 tag | `contrib/watermill-kafka/v{version}`（替代 `contrib/kafka/v…`） |

迁移时同步：`go.work`、`Taskfile.yml` release 列表、CI matrix、`_examples`、文档中所有 `contrib/kafka` 引用。

---

## 10. Watermill Bus 实现要点

1. **Start**：`go router.Run(ctx)`；允许 0 handler 启动；对外 `CheckHealth` 对齐 `IsRunning`（注意文档：历史原因下 Closed 后 `IsRunning` 仍可能为 true，关停用 `IsClosed` 或自有标志）。
2. **Subscribe**：`AddConsumerHandler` + **`RunHandlers(ctx)`**；失败返回给调用方。
3. **Publish**：经 `resolve` → Transport；wire 用 §5 单一映射。
4. **Retry / AutoAck / ContinueOnError**：行为与现 Broker 文档对齐；`Topic[T].Retry` 与 `Options.Topics[].Retry` 合并规则写清（显式 SubscribeOption > Topic > Options.Topics > 全局默认）。
5. **中间件**：Recoverer、CorrelationID 可保留；**不要** SignalsHandler。
6. **动态订阅与关停**：handler 名全局唯一；Stop 时 Close router 并关闭 Transports（生命周期约定写进文档：Transport 由谁 Close）。

---

## 11. 配置（开放）

建议配置形态（名称可审）：

```yaml
bus:
  debug: false
  log_message:
    publish: false
    subscribe: false
  retry:
    max_retries: 3
    backoff: 0s
  topics:
    order.created:
      group: order-svc
      instances: 2
      auto_ack: false
      continue_on_error: false
      route:
        transport: kafka
        key: orders   # Transport 侧键；缺省=逻辑名
  # lynx.* 禁止 route 到非内存；缺省由框架绑到内置 memory transport
  # 下面这种配置必须在 Bus.Init 失败：
  # lynx.app.started:
  #   route: { transport: kafka, key: ... }

kafka:
  orders:
    brokers: ["..."]
    topics: ["orders.v1"]
    consumer: { group_id: "...", instances: 2 }
    producer: { topic: "orders.v1" }
```

`NewFromConfig` 放在 contrib，返回已注入 Transport 与路由的 `eventbus.Bus`，供 `lynx.WithBus(...)`。

---

## 12. 测试计划

| 层级 | 覆盖 |
| --- | --- |
| 单元 | DecodeTyped / wire 往返保留 ID·Key·Headers·Time·Topic；Marshaler 优先级；Key 不被 Headers 覆盖 |
| 内存 Bus | Init 内订、订后立刻 Publish；缓冲满丢弃打日志；动态订阅 |
| 生命周期路由 | `lynx.*` 固定内存 Transport；Route 到 kafka 时 Init 失败；Register 时 `service.registered` 可收 |
| Watermill | Run 后 Subscribe + RunHandlers（含 lynx.*）；Init 内 Publish 可达；Signals 不抢占 |
| Kafka 集成（可 tag） | record key = MessageKey；多物理 topic fan-in；instances |
| 关停 | AppStopped 在 Bus.Stop 前可被订阅；组件 Stop 不关闭共享 Bus |
| 回归 | 删除 pubsub 后 `_examples/bus`、生命周期测试、现有 eventbus 测试全绿 |

---

## 13. 迁移步骤（建议实施顺序）

1. **Wire 契约 + Marshaler 对称**（核心，修错接）  
2. **Topic 方法 API + Bus 解析（Context / Default / `eventbus.WithBus`）**；`newLynx` 注入 Context 与 SetDefault  
3. **Watermill：动态 Subscribe + 提前 Start + 去 Signals**  
4. **方案 B：`lynx.*` → MemoryTransport 路由锁 + Init 校验**  
5. **`contrib/kafka` → `contrib/watermill-kafka` 重命名 + 改缝 `eventbus.Transport`**（能力对齐；删 watermill 内瘦 kafka.go）  
6. **配置装配 / RouteKey**（contrib）  
7. **删 contrib/pubsub**，改示例与文档  
8. **CHANGELOG / 发版说明**

每步可独立审查、可回滚；删 pubsub 必须在 5 完成之后。

---

## 14. 风险与开放问题（请审查拍板）

| ID | 问题 | 状态 / 建议 |
| --- | --- | --- |
| O1 | handlerName 唯一空间 | **已决（方案 B）**：单一 Bus，全局唯一即可 |
| O2 | 配置键用 `bus.*` 还是保留过渡期 `pubsub.*` 别名 | 建议：直接 `bus.*`，一次 breaking |
| O3 | `Event.Time` 缺失旧消息是否失败 | 建议：降级 `time.Now()` + Debug 日志 |
| O4 | 内存 Bus / MemoryTransport 缓冲满丢弃是否对领域过激 | 建议：领域生产用 Kafka；内存路径文档标明 |
| O5 | Transport.Close 所有权 | 建议：Bus.Stop 关闭其拥有的 Transport；所有权单一 |
| O6 | 是否保留薄 `Router` Service | 建议：可选；内部只调 SubscribeTyped |
| O7 | 生命周期走 Watermill 的转换开销 / 信封 Time | **已决（方案 B）**：接受同进程多一跳；wire 契约仍适用；payload 内业务 Time 为准 |
| O9 | `eventbus.WithBus` 与 `lynx.WithBus` 重名 | **已决**：分属两包，文档表注明；实现注释交叉引用 |
| O10 | Transport 订阅如何保留 Ack/Nack | **已决（方案 B）**：`Subscribe` 返回 `<-chan Delivery`（Event + Ack/Nack）；业务 API 仍不出现 `Delivery` / `*message.Message` |

---

## 15. 决策摘要（审查核对清单）

- [x] 对外只有 `Bus` / `Topic[T]` / `Event[T]`；删 `contrib/pubsub`
- [x] **方案 B**：生命周期走同一 Bus；`lynx.*` 强制内存 Transport，禁止出进程
- [x] **Typed API**：`Topic.Publish/Subscribe`；Bus 解析 = `WithBus` Option → Context → Default；无第二套位置参数重载
- [x] Bus 可在 Component Init 前 Start；Init 内 Subscribe 走 `RunHandlers`
- [x] 关停 Bus last-actor 不变
- [x] Wire 单点映射；Marshaler 对称；Kafka Key = 分区键
- [x] 底座 = 自研 API + Watermill 驱动箱；不用 gocloud 做内核
- [x] **`contrib/kafka` 重命名为 `contrib/watermill-kafka`**
- [x] Kafka Transport 改缝保能力；`Subscribe` → `Delivery`（Ack/Nack 转达底层）

---

## 附录 A. 实现落地对照（合入后）

| 项 | 实现 |
| --- | --- |
| Watermill 动态订阅 | `AddConsumerHandler` + `RunHandlers`；`subscriberAdapter.Close` 取消订阅 ctx |
| 生命周期 | 同一 `app.Bus()`；`lynx.*` → 内置 MemoryTransport；非内存 Route/Init 失败 |
| Typed API | `Topic.Publish/Subscribe`；`WithBus` → Context → `Default`；`newLynx` 注入 |
| Wire | `EncodeWireMetadata` / `DecodeWireMetadata`（`x-message-key` / `x-event-time` / `x-logical-topic`） |
| Kafka | `contrib/watermill-kafka`；record key = MessageKey；`Delivery.Ack` → offset |
| 删除 | `contrib/pubsub`、`_examples/pubsub`、`contrib/watermill/kafka.go` 瘦实现 |

已知可后续跟进（不挡合入）：`Run` 早退路径 Bus 关停收口、`Bus.Stop` 错误进入 `Run` 返回值、`busCancel` 加锁、`newLynx` 就绪等待可配置等。

## 附录 B. 参考

- Watermill Router： [Adding handler after the router has started](https://watermill.io/docs/messages-router/)（`AddConsumerHandler` + `RunHandlers`）
- gocloud pubsub：https://pkg.go.dev/gocloud.dev/pubsub（选型对比见 §6）
- 仓库内相关：`eventbus/`、`contrib/watermill/`、`contrib/watermill-kafka/`、`lynx.go` Run/关停、`docs/design-service-registry.md`（OnDrain 协同）
