# PubSub 架构重构设计（watermill + kafka）

> 日期：2026-08-05 ｜ 对应 `contrib/pubsub` 与 `contrib/kafka` 模块重构

## 背景与目标

现有 pubsub/kafka 架构的目标是 broker 可插拔（kafka / redis-stream / nats）且按 topic 路由到不同实现，但实现方案过于复杂：进程内中转、合成内部主题名、循环依赖、职责混杂。本次重构的目标：

1. **可插拔**：kafka / redis-stream / nats 等后端按需接入，新增后端≈零引擎代码。
2. **按 topic 路由**：一个逻辑 topic 的完整配置集中一处，路由自动推导。
3. **配置驱动**：broker 地址、物理 topic、消费组、实例数等环境相关参数全部可配置化。
4. **大幅简化**：删除 Binder、内部主题、Consumer/Producer 中转组件。

## 现状问题（研究结论）

对 `contrib/pubsub`（broker.go / broker_watermill.go / binder.go / router.go）与 `contrib/kafka`（binder.go / consumer.go / producer.go）的全面阅读结论：

1. **双重跳转**：所有消息强制经过进程内 watermill router，用合成内部主题名（`hello:kafka-consumer` / `hello:kafka-producer`）作为路由表实现手段——主题命名是字符串拼接约定，无任何强制约束。
2. **入站路由不一致（疑似 bug）**：`Consumer.Start` 用 `FromBinder()` 发布到原始事件名 `hello`，而 `Router.Run` 把 Handler 订阅到 `hello:kafka-consumer`——两边对不上。consumer 测试只针对 fake broker 单测，从未端到端验证过 Kafka→应用链路。
3. **循环依赖 + 手工接线**：`NewBroker(opts, binders)` 通过 `binder.SetBroker(b)` 自注入；Consumer 需要 broker、binder 需要 broker。示例注释"`binder.ConsumerBuilders()` 不能和 binder 同时注入"本身就是时序陷阱的证明（builder 必须等 `binder.Init` 之后才能拿到）。
4. **watermill 类型泄漏**：`HandlerFunc = func(ctx, *message.Message) error`、`NewJSONMessage` 返回 `*message.Message`——公共 API 绑定 watermill 消息模型。
5. **两套路由逻辑各写一遍**：`broker.Publish` 遍历 binder 做 `CanPublish`，`router.Run` 遍历 binder 做 `CanSubscribe`——重复且容易不同步。
6. **实体职责混杂**：Broker 同时是组件、进程内 router、binder 注册表；Binder 同时是路由表、producer 容器、consumer builder 工厂；Consumer 是"拉 Kafka 再转发进进程内总线"的中转组件。每个实体都是 `lynx.Component`，生命周期次序耦合极深。
7. **watermill 的看家本领没用上**：watermill 本身就是"pubsub 可插拔"（kafka/redis-stream/nats/amqp 各有实现，统一 `message.PubSub` 接口）。当前设计绕开它，手写 kafka-go 的 reader/writer + 内部主题方案，同时仍然依赖 watermill 的消息类型——两头不靠。

## 已确认的设计决策

1. **保留 watermill**：作为引擎直接利用其 Pub/Sub 可插拔能力；若后续设计出现新问题再重新讨论去留。
2. **中央路由表**：`topic → Transport` 映射，路由逻辑只有一处。
3. **自有消息类型**：公共 API 使用 `pubsub.Message`（ID/Key/Headers/Payload），watermill 类型只出现在 Broker 边界内部。
4. **默认 Transport**：路由表未命中的 topic 走 DefaultTransport（memory / gochannel），进程内事件零配置。
5. **物理 topic 映射在 Transport 内**：应用只写逻辑 topic 名；物理名解析（含多物理 topic 订阅）全部在 Transport 内部。
6. **配置结构**：`kafka` 段 = `map[逻辑topic]` 配置，`consumer` / `producer` 子段分离，topic 即配置 key（操作者视角单一来源，路由自动推导）。
7. **per-topic brokers**：不同事件的 broker 地址可以不同；同一 Kafka 集群内的 topic 共享客户端（实现层按 brokers 去重分组）。
8. **订阅参数双通道**：config（环境相关默认值）+ 代码显式 `WithGroup` / `WithInstances` 覆盖，优先级：显式 > config > 默认。
9. **模块路径保留、API 可破坏**：`contrib/pubsub`、`contrib/kafka` 路径不变，v0.x 阶段允许破坏性变更。
10. **核心小改动**：`lynx.Config` 接口新增 `UnmarshalKey(path string, out any) error`（viper 原生支持），用于整表加载 Transport 配置。

## 目标架构

### 模块布局

```
contrib/pubsub/  （模块不变）
  message.go      Message 类型 + 上下文工具 + watermill 互转（不导出）
  transport.go    Transport 接口 + SubscriptionOptions
  broker.go       Broker 门面（lynx.ServerLike 组件）
  router.go       Router 组件（瘦身版）
  memory.go       memory.NewMemoryTransport() —— gochannel 兜底

contrib/kafka/   （模块不变）
  transport.go    kafka.NewTransport(...) —— 唯一导出类型
```

### 核心接口

```go
// Transport：一个后端 = 一个组件。topic 参数一律是逻辑名，物理解析在内部。
type Transport interface {
    lynx.ServerLike
    Publish(topic string, msgs ...*message.Message) error      // watermill 签名，消息转换在 Broker 边界
    Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error)
    Topics() []string          // 承接的逻辑 topic 全集（自动路由用）
}

// SubscriptionOptions 是 Transport 的订阅参数；Group/Instances 已由
// Transport 按自身配置合并（代码显式值优先）。
type SubscriptionOptions struct {
    Group     string
    Instances int    // 同组消费者成员数，Transport 内部展开
}

// Broker 门面：只做查表 + 转发。
type Broker interface {
    lynx.ServerLike
    Publish(ctx context.Context, topic string, msg *Message, opts ...PublishOption) error
    Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
    Route(topic string, t Transport)
}
```

### 职责划分（对比现状）

| 职责 | 现状 | 新设计 |
|---|---|---|
| 后端连接管理 | Binder + Consumer + Producer 三个组件 | **Transport 一个组件** |
| 事件→主题路由 | Broker 遍历 Binder + Router 再遍历一遍 | **路由表唯一一处**，Broker 查表 |
| 内部主题转发 | `hello:kafka-consumer` 等合成名 | **不存在**，直连后端 |
| 物理 topic 解析 | 散落在 ConsumerOptions / ProducerOptions | **Transport 内部**（含多物理 topic 展开） |
| Handler 订阅 | Router.Start 订阅（有竞态） | Router.**Init** 订阅，Broker 缓冲、Start 统一注册 |
| 消息模型 | watermill 类型泄漏 | 自有 `Message`，转换只在 Broker 边界 |

## 详细设计

### 1. 消息类型（contrib/pubsub/message.go）

```go
type Message struct {
    ID      string            // 消息 ID（缺省随机生成）
    Key     string            // 消息 key（Kafka key / 路由键）
    Headers map[string]string // 元数据（trace-id 等）
    Payload []byte
}

func NewMessage(payload []byte, opts ...MessageOption) *Message
func NewJSONMessage(data any, opts ...MessageOption) (*Message, error)
func MustJSONMessage(data any, opts ...MessageOption) *Message   // 沿用现有 MustMarshal 风格

type MessageOption func(*Message)
func WithID(id string) MessageOption
func WithKey(key string) MessageOption
func WithHeaders(h map[string]string) MessageOption
func WithHeader(k, v string) MessageOption
```

- 转换（`toWatermill` / `fromWatermill`，不导出）只发生在 Broker 边界：`Publish` 入站时 `Message → watermill`，handler 回调时 `watermill → Message`。`contrib/kafka` 只依赖 watermill 类型（消息转换不进 kafka 模块）。
- `MessageIDKey` / `MessageKeyKey` 常量**保留**（`"x-message-id"` / `"x-message-key"`）：作为跨进程 wire 协议键（Kafka header 互通需要稳定命名），重新注释为协议键。
- 保留现有上下文工具，签名不变（tracing / 日志在用）：`MessageIDFromContext` / `ContextWithMessageID` / `MessageKeyFromContext` / `ContextWithMessageKey`。
- 删除：`SetMessageKey` / `GetMessageKey` / `SetMessageID` / `GetMessageID`（Message 字段原生承载）。

### 2. Broker 门面（contrib/pubsub/broker.go）

```go
type Options struct {
    Transports       []Transport  // 自动路由：Init 时对每个 Transport.Topics() 建路由
    DefaultTransport Transport    // 未命中路由表的 topic 走它
}

func NewBroker(opts Options) *Broker
```

- `Init(app)`：保存 app（取 logger / config 上下文）；创建 watermill router（中间件 `Recoverer` / `CorrelationID` / `Retry{MaxRetries:3}`，沿用现状）；对 `opts.Transports` 逐个调 `Route(topic, t)`（`Topics()` 全集）。
- `Route(topic, t)`：互斥锁保护的路由表写入；**显式 Route 覆盖自动路由**（kafka + nats 混合场景）；两个 Transport 声明同一 topic 的自动路由冲突 → Init 报错。
- `Publish(ctx, topic, msg, opts...)`：合并 PublishOption → `toWatermill` → 路由表命中 `t.Publish(topic, wm)`；未命中 → DefaultTransport；两者皆无 → 返回错误。
- `Subscribe(ctx, topic, handlerName, h, opts...)`：**纯缓冲**（不依赖 router 状态，可发生在 Init 前）；记录 topic / handlerName / 包装后的 handler / 合并后的订阅参数。**Start 后调用返回错误**（watermill router 运行中不允许动态注册 handler）。
- `Start(ctx)`：加锁后对每个缓冲订阅：解析 transport（路由表 → 默认 → **无归属则 Start 返回错误**，触发 run.Group 优雅关闭）；用适配器包装 transport 为 `message.Subscriber`；`AddConsumerHandler(handlerName, topic, adapter, handler)`；最后 `router.Run(ctx)`。
- `Stop(ctx)`：关闭 router。**不关闭 Transport**（Transport 由用户注册为组件，`app.Register(kafkaT)`，文档强制；未注册的 Transport 健康检查会因未 Start 而失败，可被观测）。

watermill router 与 `message.Subscriber` 的适配（内部）：

```go
type subscriberAdapter struct {
    t    Transport
    opts SubscriptionOptions
}
func (a subscriberAdapter) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
    return a.t.Subscribe(ctx, topic, a.opts)
}
```

### 3. Router 组件（contrib/pubsub/router.go，瘦身版）

```go
type Router struct { broker Broker; handlers []Handler; ... }
func NewRouter(broker Broker, handlers []Handler) *Router
```

- `Init(app)`：对每个 handler 调 `broker.Subscribe`（纯缓冲，无时序依赖——解决旧设计的注册顺序陷阱）。`HandlerOptions` 接口照旧提供 per-handler 订阅选项。
- `Start(ctx)`：阻塞至内部 ctx 取消。`Stop(ctx)`：取消。
- `Handler` / `HandlerOptions` 接口**签名不变**（`EventName` / `HandlerName` / `HandlerFunc`）。

### 4. 内存 Transport（contrib/pubsub/memory.go）

```go
func NewMemoryTransport() *Transport   // 包 gochannel，忽略 SubscriptionOptions
```

### 5. Kafka Transport（contrib/kafka/transport.go）

配置结构（定稿，`UnmarshalKey("kafka", &opts)` 整表加载）：

```yaml
kafka:
  orders:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_orders, topic_orders_v2]   # 订阅的物理 topic 列表
    consumer:
      group_id: orders-group
      instances: 3
      commit_interval: 1s
    producer:
      log_message: true
      batch_size: 100
  payments:
    brokers: ["10.0.0.2:9092"]
    topics: [payments_topic]
    consumer:
      group_id: payments-group
```

```go
type Options struct {
    Topics map[string]TopicOptions   // 逻辑 topic → 配置
}

type TopicOptions struct {
    Brokers  []string          // 必填，Init 校验（per-topic，支持不同集群）
    Topics   []string          // 订阅物理 topic 列表
    Consumer *ConsumerOptions  // nil = 该 topic 只发布不订阅
    Producer *ProducerOptions  // nil = 该 topic 只订阅不发布
}

type ConsumerOptions struct {
    GroupID        string        // 订阅组；代码 WithGroup 可覆盖，两者皆空则 Subscribe 报错
    Instances      int           // 同组消费者成员数，缺省 1
    CommitInterval time.Duration // 映射 sarama Config.Consumer.Offsets.CommitInterval（经 OverwriteSaramaConfig 透传）
    LogMessage     bool
    // 其余消费参数按 watermill-kafka v3 SubscriberConfig 字段映射
    //（ConsumerGroup / NackResendSleep / ReconnectRetrySleep 等）
}

type ProducerOptions struct {
    Topic       string // 发布物理 topic；缺省 = Topics[0]
    LogMessage  bool
    BatchSize   int    // 映射 sarama Config.Producer.Flush（经 OverwriteSaramaConfig 透传）
    // 其余发布参数按 watermill-kafka v3 PublisherConfig 字段映射
    //（OverwriteSaramaConfig 承载的 Producer 侧 sarama 参数）
}
```

实现要点：

- **引擎**：watermill 官方 `watermill-kafka/v3`（`github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka`，IBM/sarama 客户端，官方当前版本；替代现用 segmentio/kafka-go——用户已确认接受此替换）。
- **客户端分组**：内部按 brokers 集合去重分组，每组一套 watermill-kafka Publisher + 按消费组缓存的 Subscriber；同集群 topic 共享客户端，异集群各自一套。
- **`Publish(logical, msgs)`**：查 `config.Topics[logical].Producer` → 物理名（`Producer.Topic` 缺省 `Topics[0]`）→ 对应分组 Publisher。
- **`Subscribe(ctx, logical, opts)`**：查 `config.Topics[logical].Consumer`；Group = `opts.Group` 非空优先，否则 `GroupID`，再否则报错；Instances = `opts.Instances > 0` 优先，否则 `Instances`（缺省 1）。物理展开：`len(Topics) × Instances` 个组成员（每个物理 topic 各 N 个同组消费者，与旧 `Instances` 语义一致），fan-in 到单一返回 channel（watermill `Message` 无来源 topic 字段，不暴露物理 topic；跨物理 topic 无顺序保证）。组内 Subscriber 按 group 缓存复用。
- **`Topics()`**：返回 `Options.Topics` 的全部 key（自动路由用）。
- **生命周期**：`NewTransport(opts)` 构造客户端；`Init(app)` 校验 brokers 非空；`Start` 标记 running；`Stop` 关闭全部集群客户端；健康检查 = 组件 running 状态。GroupID 校验推迟到 `Subscribe` 调用时（代码 `WithGroup` 可补位，Init 阶段无法预知）。

### 6. 数据流（最终形态）

```
出站: app → broker.Publish("orders", msg)
        → 路由表: "orders" → kafkaT
        → kafkaT.Publish("orders") → 物理 "topic_orders_v2" → sarama → Kafka
       （无进程内跳转，无 Producer 组件）

入站: Kafka topic_orders / topic_orders_v2 → kafkaT.Subscribe("orders")
        （组 orders-group × 3 实例 × 2 物理 topic，fan-in）
        → broker 的 handler 包装（ctx 注入 / Retry×3 / Ack 语义）
        → 用户 handler
       （无 Consumer 中转组件，无内部主题名）

进程内: broker.Publish("local.event") → 路由表未命中
        → DefaultTransport = memory（gochannel）→ handler
```

### 7. 错误处理与启动校验

| 场景 | 行为 |
|---|---|
| Publish 路由未命中且无默认 Transport | 返回错误 |
| 订阅 topic 无归属 Transport | `Broker.Start` 返回错误 → run.Group 触发优雅关闭（启动即暴露，不静默丢消息） |
| kafka `Brokers` 为空 | `kafkaT.Init` 返回错误 |
| 订阅 topic 的 `GroupID` 缺失（代码 `WithGroup` 与配置皆无） | `kafkaT.Subscribe` 返回错误 → `Broker.Start` 报错 |
| 处理失败 | Retry×3 → Nack；`ContinueOnError` → Ack 继续；`AutoAck` → 先 Ack 再执行（at-most-once） |
| Kafka 连接中断 | sarama 内部重连；fetch 错误由 watermill 层重试 |

### 8. 生命周期与健康检查

- **Transport**：构造即建客户端；`Init` 校验配置；`Start` 标记 running；`Stop` 关闭客户端。**必须由用户注册为组件**（`app.Register(kafkaT)`），Broker 不代管其生命周期。
- **Broker**：`Init` 建 watermill router + 自动路由；`Start` 统一注册缓冲订阅后 `Run`；`Stop` 关 router。
- **Router**：`Init` 订阅（纯缓冲，安全）；`Start` 阻塞。
- 健康检查：Broker 查 router 运行态；Transport 查自身运行态。多集群时每个 Transport 独立上报。

### 9. 核心模块小改动（lynx/config.go）

`lynx.Config` 接口新增一个方法（viper 适配器原生支持，v0.x 允许）：

```go
// UnmarshalKey 将 path 对应的配置子树解码到 out 指向的结构体。
UnmarshalKey(path string, out any) error
```

### 10. 测试策略

- **contrib/pubsub**：
  - Message 转换 round-trip（ID/Key/Headers/Payload 双向一致）
  - memory transport + broker 端到端（现有 `startBroker` 模式改造）
  - 路由：Route 命中 / 默认回退 / 无归属报错（Start 期）
  - 订阅语义：AutoAck / ContinueOnError / Retry（现有测试直接迁移）
  - 缓冲订阅：Init 前 / Init 后 / Start 后注册（Start 后报错）
- **contrib/kafka**：
  - 配置解析：per-topic 默认与覆盖（Brokers 继承、GroupID/Instances 缺省、Producer.Topic 缺省 = Topics[0]）
  - 物理展开：多物理 topic × instances 的组成员数
  - 组路由：同组共享 Subscriber、异组分离
  - 生命周期与健康检查；Init 校验（brokers / GroupID 为空报错）
  - 对底层客户端注入 fake 的单元测试沿用现有 seam 思路
- **未来新后端**：watermill 官方 `pubsub/tests` 一致性测试套件可整套复用——"可插拔如何验证"的标准答案。
- `_examples/pubsub` 重写为真实端到端演示（含 config.yaml 加载路径）。

### 11. 迁移清单

**删除**：
- pubsub：`Binder` 接口、旧 `NewBroker(opts, binders)` 签名、`FromBinder`、`OnlyFirstBinder`、`SetMessageKey` / `GetMessageKey` / `SetMessageID` / `GetMessageID`、旧 `NewJSONMessage`（watermill 返回）
- kafka：`Binder`、`Consumer`、`ConsumerBuilder`、`Producer`、`Handler`、`NewConsumer`、`NewProducer`、`NewConsumerBuilder`、`NewKafkaMessage`、`NewKafkaMessageJSON`、`GetMessageID`、`ToConsumerName`、`ToProducerName`、旧 `ConsumerOptions` / `ProducerOptions`
- 旧 `ConsumerOptions` 的 `MappedEvent`（map key 即逻辑 topic）、per-entry `Brokers`（上提到 topic 级）

**保留（签名不变）**：`Handler`、`HandlerOptions`、`WithAutoAck`、`WithContinueOnError`、context 工具函数

**新增**：`Message` 及构造器、`MessageOption`、`Transport`、`SubscriptionOptions`、`Options{Transports, DefaultTransport}`、`NewBroker(Options)`、`Route`、`WithGroup`、`WithInstances`、`memory.NewMemoryTransport`、kafka `Options` / `TopicOptions` / `ConsumerOptions` / `ProducerOptions` / `NewTransport`；核心 `lynx.Config.UnmarshalKey`

**依赖变化**：`contrib/kafka` 的 `segmentio/kafka-go` → `watermill-kafka/v3`（IBM/sarama 间接依赖）；`contrib/kafka` 仍依赖 `contrib/pubsub`（Transport 接口）与 watermill。

### 12. 未来扩展（本次不做）

- redis-stream / nats：各一个 contrib 模块，实现 `Transport` + `Topics()` 即可接入，可复用 `pubsub/tests` 一致性套件
- 前缀 / 通配路由（`RoutePrefix`）：路由表扩展点，YAGNI
- kafka 级默认 brokers 段：若同集群 topic 增多，可加"默认 brokers + topic 级覆盖"，纯配置扩展不动结构

## 实施偏差记录（2026-08-05，Task 6 同步）

实施与上述设计的差异，均为不影响目标架构的小偏差：

1. `SetMessageKey` / `GetMessageKey` / `SetMessageID` / `GetMessageID` 未按第 11 节迁移清单删除，保留为 `Deprecated`（`broker.go`），公共 API 由 `Message` 字段与 `WithKey`/`WithID` 承载。
2. `NewBroker(opts)` 返回未导出的 `*broker` 具体类型（设计写的是导出的 `*Broker`）；`Broker` 接口本身公开导出，调用方以接口形态使用，无实质影响。
3. kafka 客户端（publisher/subscriber）为**懒创建**：首次 `Publish`/`Subscribe` 时才经工厂建立并按 brokers/组缓存，而非 `NewTransport` 时构造（设计 5 节"生命周期"）。
4. `ConsumerOptions` / `ProducerOptions` 仅实现设计列出的字段子集（`GroupID`/`Instances`/`CommitInterval`/`LogMessage` 与 `Topic`/`LogMessage`/`BatchSize`），watermill-kafka v3 的其余 sarama 参数未透传。
5. `PublishOptions` 额外提供 `WithMetadata` / `WithMetadataField`（合并进消息头）；handler 回调会注入 message ID/key 上下文（`ContextWithMessageID`/`ContextWithMessageKey`），设计未列。
6. 内存 Transport 返回 `*MemoryTransport` 具体类型（设计写 `*Transport`），`Topics()` 返回 nil（仅作默认回退，不参与自动路由）。
