# Changelog

## Unreleased

### 破坏性变更：registry / cluster 契约收敛（所有权、post-close、租约）

- `Registrar.Stop` 默认不再关闭传入的后端（谁构造谁负责）：共享给 Resolver
  的后端不再被连带关掉；确需由 Registrar 释放时显式
  `WithCloseBackendOnStop()`。
- 导出 `registry.ErrClosed`：Close 后 Register/Deregister/Heartbeat/Watch/
  GetService 一律返回它；memory 的 GetService/Heartbeat 从「继续服务/恒
  nil」改为拒绝；Consul 的 Heartbeat no-op 分支同样拒绝；Close 幂等。
  memory 的 GetService/Watch 另补空名校验（`ErrBadName`，与 DNS/Consul 一致）。
- 导出 `cluster.ErrLeaseLost`：memory/redis 续约丢失返回它；Consul 对缺失
  Session（404）映射为它，网络错误原样返回。cluster memory 的 Release
  去掉「ctx 已取消即跳过」预检：先取消续约并尽力释放（本地释放恒成功）。
- DNS 新增 `WithDNSResolver`（`DNSResolver` 窄接口）：自定义解析器与测试
  替身注入；`withDNSLookup` 保留为内部别名。
- 新增 conformance 套件：`contrib/registry/registrytest`（Registry/
  Discovery/Watcher）与 `contrib/cluster/clustertest`（Coordinator），接入
  memory/DNS/consul 与 cluster memory/cluster-redis/consul coordinator；
  套件放接口旁（design-testkit 约定 lynxtest 零 contrib import）。

迁移：依赖 Registrar.Stop 释放后端的调用方加 `WithCloseBackendOnStop()`；
依赖 memory Close 后继续读的调用方改为在 Close 前取快照。

### 变更：请求标识解析与传播统一（internal/propagation；gRPC 补齐生成/回写）

- 新增中性 `internal/propagation`：wire 键、值校验、日志属性与出站传播的
  唯一归属；`serverkit` 转发并提供 `ResolveInbound`（合法沿用 / request_id
  缺失或非法生成 UUID / user_id 非法丢弃）；`client/http` 与 `client/grpc`
  只保留传输写入形态（提取、键映射、不覆盖裁决单点）
- gRPC 服务端行为对齐 HTTP：解析后生成/回写 request_id（响应 metadata），
  新增 `WithDisableRequestID`；`server/grpc` 与 `client/grpc` 导出
  `RequestIDHeader`/`UserIDHeader` 常量
- eventbus 消息头传播纳入同一校验：非法的 request_id/user_id 值不再写入
  消息头、消费侧不还原（自定义 `PropagateAttrs` 键不受影响）
- 修正 docs/05 与 Recovery/RequestID 注释中的链序表述（默认装配 RequestID
  包在用户中间件外侧，panic 日志带 request_id），补顺序钉子与 gRPC
  client→server 闭环测试

行为变更：gRPC 服务端开始生成并回写 request_id（可关）；gRPC 响应
metadata 新增 x-request-id；非法请求标识值在 bus 路径不再传播/还原。

### 变更：server 生命周期收敛为 serverkit.Lifecycle（HTTP/gRPC/debug）

三个适配器各自复制的生命周期状态机收敛为 `internal/serverkit.Lifecycle`
（唯一归属）：Start 重入守卫、stopRequested、ready 信号与 `lynx.server.*`
事件发布（`ServerEvent.Service` 构造时固定，不再手拼字符串）；适配器保留
listener/server 创建、`Serve`/`GracefulStop`、地址策略、健康轮询与启动期
交错处理。行为变更：

- debug 补齐 SC-14 重入守卫：二次 Start 从「覆盖 httpServer/listener
  泄漏旧 listener」变为报错；`Init` 复位守卫与停止标志（此前 `stopping`
  一次性，重新 Init 也无法再 Start）。
- debug 实现 `AdvertiseAddr() string`（空串），满足 `lynx.Server`，
  `lynxtest.WaitReady` 等统一消费。
- HTTP/debug 共用的 `Shutdown + http.ErrServerClosed 归一化` 收进
  `serverkit.ShutdownHTTP`。
- HTTP/gRPC 行为不变（守卫、事件、关停语义逐项保留）；serverkit 仍为
  internal，无公开 API 破坏。

测试：Lifecycle 单测（守卫/复位/ready once/事件与 Service 名/nil bus）、
debug 二次 Start 与重新 Init 可 Start 钉子、HTTP/gRPC 生命周期事件序列
测试。

### 变更：readiness 收敛为单一 probe（ready.go）

`ready.go` 新增内部 `probe` 值（`resolveProbe` + `wait` / `once`）作为就绪
解析与探测的唯一归属：三级解析（Ready → Checker → 无信号即就绪）、startErr
交错（peek 放回）、ctx 取消优先与预算边界都在此统一；四个消费方
（OrderedServices、newLynx 总线就绪等待、Command、组健康）不再各自拼接
探测协议。行为变更：

- Command 的取消语义收紧：**ctx 取消优先于探测结果**——取消后立即中止
  （`aborted`），不再出现「已取消但探测成功继续执行」（此前 Checker 层用
  Background 基 ctx 规避取消）。
- Command 新增可选墙钟总预算 `WithWaitBudget(d)`（默认 0 = 不限，行为
  不变）：与 `MaxTries`/`WithBackoff` 的轮次预算取先到者；`docs/04` 同步
  修正「`MaxTries`/`WithBackoff` 仍是总预算」的旧表述。
- 新增 `NewOrderedServices(name, services []Service, opts ...OrderedOption)`
  与 `WithOrderedReadyTimeout(d)`：每子服务就绪预算可配（默认 10s 不变）；
  `OrderedServices(name, services...)` 保留为默认委托，源码兼容。
- 总线就绪等待改走 `probe.wait`（传入 Start 失败通道），删除调用点手写的
  select/goroutine；行为不变。
- 无就绪信号子服务的「快速失败不阻止下一个子服务启动」语义文档化并补钉
  （可能短暂启动后随组失败回收）。

测试：probe 表驱动单测（三级 × wait/once × 取消/startErr/预算/挂死）、
OrderedServices 第三层与健康/就绪区分钉子、Command 预算与取消钉子。

### 变更：Watcher 会话语义收敛为 WatcherSession（contrib/registry）

`registry.WatcherSession`（组合 `WatcherBase`）成为后端 watcher 的会话核心：
后置 `MatchFilter`、全字段顺序无关的规范相等、首快照基线与拉模式退避节奏
（轮询 / 长轮询）统一在此；memory / DNS / Consul 只提供查询闭包与节奏策略。
行为变更（可观察）：

- 快照规范化为唯一形态（实例按 ID、Endpoints/Tags 排序）；与已投递快照
  规范相等的变化不再推送——memory 无变化写入、Consul 内容无变化的 index
  跳变不再唤醒消费者。
- DNS 始终解析全部已配置协议：协议过滤下返回全量 Endpoints（不再修剪），
  与 memory/consul 形状一致；NXDOMAIN 空快照语义与负缓存钳制 [5s,30s] 不变。
- `Resolver.Subscribe` 推送深拷贝规范副本（与 `Get` 的「快照副本」契约
  一致，此前明确不深拷贝）；与已投递快照相等的变化不重复推送；stale 丢弃
  仍不通知（last-known 语义，恢复后下一次 store 自动推送）。
- Consul index 回绕恢复语义不变；回绕重查到的旧快照被相等抑制，不再产生
  一次重复推送。

适配器侧退避（DNS 负缓存、Consul 1s–30s 倍增）由会话核心的 `PullPolicy`
承载；Resolver 外层重连保留为契约级「不可恢复错误」安全网。文档同步：
design-service-registry.md / design-resolver-subscribe.md / 07-registry.md。

### 破坏性变更：订阅配置收敛为 ResolvedSubscription（单一解析产物）

`eventbus.Resolver` 新增 `ResolveSubscription(topic, opts) ResolvedSubscription`：
把有效 handler 名（空 → topic）、订阅级 `MaxInFlight` 与每 handler 的
`Retry` / `HandlerTimeout` / `AutoAck` / `ContinueOnError` 的合并收敛到唯一
入口，**订阅时一次解析**；memory Bus 与 watermill Bus 的投递路径不再逐次
解析（对齐 CORE-03 解码器订阅时解析的先例）。删除 `Resolver.ApplyTopicDefaults`
/ `RetryFor` / `HandlerTimeoutFor`（无兼容别名）。迁移对照：

| 旧 | 新 |
|---|---|
| `ApplyTopicDefaults(topic, o)` | `res := ResolveSubscription(topic, *o)`（不再原地修改 `SubscribeOptions`） |
| `RetryFor(topic, call)` | `ResolveSubscription(topic, SubscribeOptions{Retry: call}).Retry` |
| `HandlerTimeoutFor(topic, call)` | `ResolveSubscription(topic, SubscribeOptions{HandlerTimeout: call}).HandlerTimeout` |

语义不变：优先级链（调用/Topic 携带值 > `Options.Topics` > 全局 > 默认）、
`HandlerTimeout` 负值显式禁用、`MaxInFlight` 负值不限制均原样保留；同名事件
的两个 handler 解析出不同 `MaxInFlight` 时仍为首值生效 + Warn（比较发生在
解析值上，相等不再误报）。设计文档已同步（§4.2 / §5.2 / §10.4 / 附录 A）。

### 变更：启动期早退与 Close 的关停契约收敛（关停快路径）

启动期早退（Init 失败、排水钩子未配预算、配置热更新注册失败、OnPreStart
失败）与 `Run()` 从未启动时的 `Close()` 现在共用同一关停执行器（`teardown`，
`shutdown.go`）——停止语义与错误记账只有一处实现，补齐此前只有完整关停
序列才兑现的文档承诺：

- `Run()` 在早退路径返回**触发错误与关停错误的聚合**（`errors.Join`）：
  服务 `Stop` 的错误（含超时）不再只记日志；`errors.Is/As` 仍可命中触发错误。
- 早退与 `Close` 逆序有界停止已 Init 的服务（`Close` 此前只停总线）；
  批次幂等——`addServices` Init 失败已停止的服务不会被重复 `Stop`。
- `Close` 的总线停止改走有界路径：错误进入聚合并记 Error 日志（此前 `_ =`
  静默丢弃），总线生命周期事件与其它路径对称。
- 应用 Context 在早退/`Close` 返回前取消；快路径不执行排水与 `OnPreStop`
  （服务未进入运行阶段），`OnPostStop` 仍恰好执行一次；从未 `Start` 过的
  服务不发 `lynx.service.stopping/stopped` 事件。

行为变更：`Runner` setup 回调失败经 `RunE → Close` 释放时，已注册服务的
`Stop` 会被调用（此前不会）；早退路径的错误串会多出服务停止错误片段。
契约见 [docs/03-core-concepts.md](docs/03-core-concepts.md)「启动期早退的
关停契约」与 [CONTEXT.md](CONTEXT.md)「关停快路径」。

### 破坏性变更：消费模型——订阅单元从 handler 收敛为事件

同一事件的多个 handler 不再各自建立 transport 订阅：它们共享该事件的一条
订阅并进程内并行扇出（每个 handler 都收到每条消息）。消费组 / 消费者成员数
是后端配置，Bus 不再建模。变更点：

- 删除 `eventbus.WithGroup` / `WithInstances` / `WithTopicGroup` /
  `WithTopicInstances`：组与成员数只存在于后端配置（kafka
  `consumer.group_id` / `consumer.instances`）；Bus 层事件配置只剩
  `max_in_flight`（原 `concurrency` 更名，默认 1 = 串行且保序）。
- 删除 `eventbus.GroupClaims` / `EffectiveGroup` / `DefaultGrouper`、
  `Transport.DeliveryMode`（含 `DeliveryBroadcast` / `DeliveryConsumerGroup`）
  与 kafka `DefaultGroup`：订阅复用键退化为逻辑 topic，"两个 handler 共组
  瓜分分区"结构上不再可能，`Subscribe` 不再因此报错。
- 共享 offset 的失败语义：全部 handler 成功才 Ack；任一终态失败整条重投
  （成功过的 handler 也会重跑，业务需幂等）；某 handler 超过
  `max_redeliveries` 后被跳过并记 Error，不连坐其他 handler。
- `AutoAck` 语义微调：不再"先 Ack 后执行"，而是"不参与整条消息的确认裁决"。
- 新增**订阅级在途上限** `bus.topics.<t>.max_in_flight`
  （`Topic.WithTopicMaxInFlight`）：默认 **1** —— 同一事件订阅串行处理
  （有界，goroutine 上界 ≈ handler 数 + 2，并恢复同订阅投递顺序；此前
  watermill 路径是 router 每消息一 goroutine、无上限无背压）。调大即并发
  处理；负数 = 不限制（逃生口，不推荐）。限流点在适配器（交给 router 前
  占槽），形成对 transport 的真背压。

迁移：删掉 `Subscribe` 上的 `WithGroup` / `WithInstances`；组 / 成员数移到
kafka 段（`consumer.group_id` / `consumer.instances`）；`concurrency` 改名
`max_in_flight`；自定义 Transport 删除 `DeliveryMode()` 方法。吞吐受默认串行
影响的主题按需调大 `max_in_flight`。设计与理由见
[docs/design-eventbus-consumption.md](docs/design-eventbus-consumption.md)。

### 破坏性变更：`registry.WatcherCore` 更名为 `registry.WatcherBase`

`Core` 暗示唯一核心实现，实际是各后端 watcher 与消费侧订阅复用的共享
骨架（典型用法为内嵌）；更名为 `Base` 与定位一致，不留兼容别名。受影响
模块：`contrib/registry`、`contrib/consul`。迁移对照：

| 旧 | 新 |
|---|---|
| `registry.WatcherCore[T]` | `registry.WatcherBase[T]` |
| `registry.NewWatcherCore` | `registry.NewWatcherBase` |

方法与语义不变：`Next` / `Receive` / `Push` / `Drain` / `Stop` / `Done` / `Ctx`。

### 新增：`wmkafka.NewBusFromConfig`——Kafka 版总线装配入口

`contrib/watermill-kafka` 新增 `NewBusFromConfig(cfg)`（`eventbus.Bus` +
`[]lynx.Service` + error）：返回值签名与 `lynx.WithBusProvider` 直接兼容。
装配语义为既有示例/文档中 `busFromConfig` 胶水的沉淀——始终提供
`"memory"` transport（兼作 DefaultTransport，承接 `lynx.*` 与未 route
的 topic）；`kafka:` 段启用时构建 Transport、加为 `"kafka"` route 并作为
配套服务返回（框架托管生命周期）；段缺失或为空时为纯内存总线（配置即
开关）。自定义 transport 集合仍走 `watermill.NewFromConfig(cfg, transports)`
手工装配；示例与文档已收敛为一行接入。

### 新增：`lynx.NewHandlerService`——事件 handler 服务适配器

订阅型 handler 的注册样板（声明主题 / handler 名 / 处理函数、Init 订阅、
Start 等待关停）沉淀为 `lynx.HandlerService[T]` + `lynx.NewHandlerService`：
业务结构体实现 `lynx.EventHandler[T]`（`Topic` / `HandlerName` / `Init` /
`Handle`），适配器保证**先 `Init` 注入依赖、再订阅**——`Init` 可用
`AppContext` 取构造期拿不到的配置/日志，返回错误则不订阅。handler 级选项
（`WithSubscribeRetry` / `WithAutoAck` / `WithContinueOnError`）在注册点
透传；默认 handler 名 = `HandlerName()`，显式 `WithHandlerName` 可覆盖。
`Name()` 构造后即可用（框架可能在 `Init` 前调用），`Start` 无需手写
`WaitForShutdown`。

```go
type OrderCreatedHandler struct{ name string; db *sql.DB }

func (h *OrderCreatedHandler) Topic() eventbus.Topic[OrderCreated] { return OrderCreatedTopic }
func (h *OrderCreatedHandler) HandlerName() string                { return h.name }
func (h *OrderCreatedHandler) Init(ctx lynx.AppContext) error     { return nil } // 依赖注入点
func (h *OrderCreatedHandler) Handle(ctx context.Context, e *eventbus.Event[OrderCreated]) error { ... }

app.Register(lynx.NewHandlerService(&OrderCreatedHandler{name: "order-created", db: db}))
```

`_examples/bus-kafka` 已改为该形态；同一事件的多个 handler 共享一条
transport 订阅并进程内并行扇出（见上条消费模型）。

### 新增：handler 超时——挂死 handler 的止损闭环

`bus.handler_timeout`（全局）与 `bus.topics.<t>.handler_timeout`（主题级，
负值 = 显式禁用）为 handler **单次尝试**设置执行上限，默认 0 = 不限制。
超时按本次尝试终态失败处理：重试 → 重投 → 毒消息止损，防止挂死的 handler
永久占用订阅级在途槽位（`max_in_flight=1` 时整个订阅停摆）。实现归共享
执行点 `eventbus.InvokeHandler`（截止 ctx + 看门狗），内存 Bus 与
watermill 路径同时生效；`Topic.WithTopicHandlerTimeout` 提供编程式入口。

注意：Go 无法终止 goroutine——handler 不尊重 ctx 时，超时只释放调用方，
handler goroutine 仍会运行到自行返回（可能与被重投的尝试重叠执行）。

### 新增：总线消息 trace 上下文传播（W3C traceparent）

跨进程 Bus 追踪不再断链：发布侧 `BuildRawEvent` 在组装末尾用全局 propagator
把当前 active span 注入事件头（`traceparent` / `tracestate`）；消费侧
`InvokeHandler` 提取远端上下文并开 `consume <topic>` span（SpanKind=Consumer，
附带 `messaging.destination.name` / `messaging.message.id`，覆盖全部重试尝试，
终态失败记 error 状态）。

- **零行为变化**：未接入 OTel（全局 tracer/propagator 为 no-op）时既不写头也
  不开 span；无远端上下文的消费不新增 span。
- **接入即生效**：`contrib/telemetry` 托管时自动设置 TraceContext+Baggage
  propagator；手动接入需自行 `otel.SetTextMapPropagator`。
- 与 `PropagateAttrs` 的日志属性白名单（request_id/user_id）互相独立；显式
  带入的 traceparent 在无 active span 时保持原值（桥接场景）。详见
  [docs/design-eventbus.md](docs/design-eventbus.md) §5.7。

### 新增：telemetry 配置驱动与 OTLP 一等选项

`contrib/telemetry` 新增 `NewFromConfig(cfg, opts...)`：按 `telemetry:` 段装配
（段缺失 / 空段 = 全默认），非法配置在装配期报错、调用方 `opts` 最后覆盖。

- **trace**：`exporter: noop|stdout|otlp`（OTLP/gRPC）、`sampling_ratio`
  （ParentBased TraceIDRatioBased；省略 = SDK 默认）、`otlp.*`
  （endpoint / insecure / headers / timeout / compression）。
- **metric**：`exporter: prometheus|otlp`、`interval`、`otlp.*`。
- 新增编程式选项 `WithTraceSampler`；依赖 `otlptracegrpc` /
  `otlpmetricgrpc` v1.45.0（与根模块 otel 版本对齐）。
- 配置见 [contrib/telemetry/README.md](contrib/telemetry/README.md)。

### 新增：Kafka consumer lag 指标导出

`contrib/watermill-kafka` 新增 `kafka.metrics`（保留键）配置：显式启用后按
`interval`（默认 30s）采集「高水位 − 已提交 offset」，以 OTel Int64Gauge
`lynx.kafka.consumer.lag` 导出，属性为物理 topic / 消费组 / 分区。默认关闭
（显式启用——采集会周期性查询 broker）；未提交 offset 的分区跳过，采集失败
只记 Warn 并继续；采集连接按（brokers × 认证）共享、随 Transport 停止关闭。
见 [contrib/watermill-kafka/README.md](contrib/watermill-kafka/README.md)。

### 新增：`/metrics` 一等挂载（WithEndpoint + telemetry.PrometheusHandler）

`server/http` 新增 `WithEndpoint(path, handler)`：运维端点独立挂载，不经过
业务中间件、request log 与 otel instrumentation（与内置健康端点同一取舍，
抓取/探针流量不产生自引用指标与日志噪声）；路径非法、nil handler、与健康
端点或彼此冲突（重复 / 模式重叠）在 `Start` 期报错而非 panic。

`contrib/telemetry` 新增 `PrometheusHandler()`（默认注册表 `promhttp.Handler()`），
与默认 Prometheus reader 配套——一行挂载：
`http.WithEndpoint("/metrics", telemetry.PrometheusHandler())`。核心模块不引入
prometheus 依赖（handler 归属 telemetry contrib）。示例 `_examples/http` 已改为
该挂载方式；见 [docs/05-servers.md](docs/05-servers.md) §5.4.4。

### 测试：Kafka testcontainers 集成测试（WK-19）

`contrib/watermill-kafka` 新增 `//go:build integration` 冒烟：testcontainers
启动 confluent-local，验证 v1.16 消费模型——同一事件的两个 handler 共享一条
transport 订阅、各自收到全部消息；消费组 / 成员数只来自模块配置；发布走
类型化 Topic 的完整 wire 路径。Docker 或镜像不可用时自动跳过（`t.Skipf`）；
运行：`go test -tags integration ./...`。测试依赖新增 testcontainers-go
v0.41.0（仅测试路径；该版本不抬高仓库现有 otelhttp 版本）。

另以 `TestIntegrationPerPartitionOrder` 钉住每分区消费性质，并**撤销此前
文档中的「Kafka 提交乱序窗口」论断**：watermill-kafka 的 `ConsumeClaim`
同步执行 `processMessage`（等 `Acked()` 才取下一条），同一分区同时仅一条
未确认消息——同分区内确认 / 提交天然严格有序；`max_in_flight` 的并发只体现
在跨分区 / 跨物理 topic（以及内存 Bus）。

### 新增：`Event[T]` 实现 `slog.LogValuer`——事件日志结构化可读

订阅方直接 `logger.InfoContext(ctx, "recv order created", "handler", name,
"event", e)` 即输出结构化字段：TextHandler 为 `event.id=... event.topic=...`
独立键值对（此前回落 `fmt.Sprintf("%+v")`：整段 Go 语法、字段不可单独检索），
JSONHandler / zap 桥接为 `"event":{...}` 嵌套对象，可按 `event.id` /
`event.topic` 过滤聚合；不再需要调用方 `json.Marshal` 转字符串或 spew 调试
打印。字段名与 JSON 标签一致、nil 指针安全；`LogValue` 在 handler 真正写
记录时解析（级别未启用零开销），payload 不可序列化时就地降级、不丢整条
记录。`_examples/bus-kafka` 的 `logOrder` 同步回归一行调用。

## v1.15.0 (2026-09-24)

本次发布 tag：根 `v1.15.0`、`contrib/watermill/v1.8.0`、
`contrib/registry/v1.10.0`、`contrib/schedule/v1.10.0`、
`contrib/consul/v1.9.0`、`contrib/cluster/v1.2.0`、
`contrib/cluster-redis/v1.2.0`、`contrib/telemetry/v1.8.0`。
`contrib/zap` 与 `contrib/watermill-kafka` 本批无源码变更，不重复打 tag。

本批为架构评审报告（2026-09-23）候选 1-8 全量落地与剩余小项收敛：
应用生命周期自有关停调度、readiness 有界收敛、Bus 共享核心再下沉、
server 共享规则（serverkit）、Watcher 骨架与订阅契约收口、
claim/毒消息止损下沉、时间源接缝、lynxtest 补全为唯一测试上下文。

**应用迁移总览**（多为机械替换）：

- `Register`/`RegisterFactories`/`Command`：`Close` 之后同样被拒绝
  （此前迟到的 Register 产生的服务永不 Stop）；`App.Command` 支持
  `CommandOption` 变参；
- `Topic.PublishRaw(ctx, raw)` → `Topic.Publish(ctx, raw)`（等价面收敛）；
- `Resolver.Subscribe(name)` → `Resolver.Subscribe(name, filter)`
  （返回已过滤快照，消费方删除自行 `MatchFilter`）；
- `cluster.RunRenewLoop(..., renew)` → 末尾增加 `clk lynx.Clock` 参数
  （适配器经 `cluster.ClockFrom(opts...)` 获取）；
- 事件订阅：`lynx.http.*` / `lynx.grpc.*` 六个主题 → 统一的
  `lynx.server.listening` / `stopping` / `stopped`（`ServerEvent.Service`
  区分 http/grpc/debug）；
- 传播 wire 键统一为 `x-request-id` / `x-user-id`（HTTP 头与 gRPC
  metadata 同源；gRPC 原为 `request_id` / `user_id`）；
- `telemetry.Options` / `schedule.Options` 未导出（无外部消费路径）。

### 破坏性变更：应用生命周期自有关停调度（移除 oklog/run）

新增 `lifecycle.go` 作为 actor 调度、关停阶段序列与注册状态机的唯一归属：
任一触发（服务返回 / 信号 / Close / OnPostStart 错误）进入同一序列——
drain → cancelCtx → OnPreStop → **逆序**停止服务（LIFO）→ AppStopped →
有界停总线 → 一次性聚合错误；阶段顺序不再由 actor 注册顺序决定（修复
Start 失败路径上 OnPreStop 排在服务 Stop 之后的倒置）。总线 Start 错误
作为构造期根因快速失败、Stop 错误进入 Run 返回值；启动期早退路径不再
泄漏总线；`oklog/run` 依赖移除。

### 破坏性变更：readiness 有界收敛

`ready.go` 收编三级解析（Ready → Checker → 放行）与两种消费模式
（Command 单次有界探测 / OrderedServices 预算循环）：Ready 等待与
CheckHealth 调用都不再越过预算，取消优先。修复两处预算失效：OrderedServices
的 Ready 通道路径此前无界（可永久卡启动）；总线就绪等待挂在
`context.Background()` 上（挂死 checker 使 `newLynx` 永不返回）。HTTP/gRPC
重复的 `runHealthChecks` 与两个 `DefaultHealthCheckTimeout` 常量删除。

### 破坏性变更：Bus 共享核心再下沉

- 新增 `eventbus.Resolver`（marshaler/retry/log-message/传播键解析与 Topic
  默认合并的唯一归属）与 `eventbus.InvokeHandler`（handler ctx、固定退避
  重试、AutoAck / ContinueOnError 裁决的唯一执行点）；memory 与 watermill
  的复制实现删除，ack 时序（AutoAck 先 Ack）留在适配器；
- 新增 `eventbus.GroupClaims`（消费组占用）与 `eventbus.RedeliveryLimiter`
  （毒消息止损计数）：watermill 只接线，配置解析留在适配器；
- `Options` 上的 `RetryFor` / `LogMessageFor` / `PropagateKeys` 与
  `ApplyTopicConfig` 收编进 Resolver；选错后端的选项
  （Transports/Debug/BufferSize、group/instances）改记 Warn 不再静默；
- `WithMetadata` 克隆调用方 map（此前 `WithMetadataField` 会反向污染）。

### 破坏性变更：server 共享规则收敛至 internal/serverkit

- 健康执行、有界关停、请求标识、生命周期事件四规则的唯一归属；关停统一
  min(调用方, 配置)，超时先 force 解除阻塞再等 graceful 退出；
  debug 新增 `WithShutdownTimeout`（默认 3s）；
- HTTP 服务端**默认安装** request_id/user_id 传播（`WithDisableRequestID`
  关闭），补齐此前 user_id 断链；gRPC metadata 键改 `x-request-id` /
  `x-user-id`（同时消解 Envoy 等代理对下划线 header 的默认拒绝）；
  client/http 常量去重；两包 `RequestIDFrom` 委托共享实现；
- 生命周期事件收敛为一组 `lynx.server.*` 主题，debug 接入。

### 破坏性变更：Watcher 骨架与订阅契约收口

- 新增泛型骨架 `registry.WatcherCore[T]`（首次快照注入、缓冲 1 最新替换、
  Stop 幂等 + 注销钩子；停止/取消优先于挂起推送）；memory/dns/consul
  三个后端与 resolver 订阅共用，四套手写循环收敛；
- `Resolver.Subscribe(name, filter)` 返回已过 `MatchFilter` 的快照（与
  Watch/GetAll 读路径一致；缓存仍按服务名共享一条）；grpc_resolver 的
  实例级补偿过滤删除；
- sentinel 统一为 `registry.ErrWatcherStopped`（删除 registry/consul 私有副本）。

### 变更：时间源接缝（`lynx.Clock`）

新增公开 `lynx.Clock`（Now + After）与 `internal/clock`（Real 生产 /
Fake 可控）：`cluster.WithClock` 使续约等待与内存 TTL 判定可确定性推进；
`registry.WithResolverClock` 覆盖缓存 updatedAt / stale 判定。TTL 边界、
续约刻度、stale 边界的测试改为假时钟断言（cluster-redis 删除
FastForward+sleep 混合时钟）；schedule 既有的 `WithNow` 保持。

### 变更：lynxtest 补全为唯一测试上下文

`NewContext` 新增 `ContextWithMeta` / `ContextWithCheckers`，默认携带稳定
Meta `{test-service, test-instance}`；新增 `lynx.ContextWithMeta`（Meta 的
对称写入口）。七个手写 AppContext 替身（consul/schedule/telemetry/zap/
watermill-kafka/registry + command 包外用例）迁移删除；App 级注册协议
替身与 debug 控制面保留为文档化例外。

### 变更：schedule identity 与引擎 parser 同源

`fireIdentity` 改用引擎实际解析的 `cron.Schedule`（注册时经 `Entry(id)`
回读）：`WithCron` 自定义 parser 时不再分叉（5 字段 parser 此前会直接
报错）；`@every` 经 `ConstantDelaySchedule` 类型识别；私有
`exclusiveParser` / `parseEvery` 删除。`Topic.Publish` 的原始载荷分支
（`*RawEvent` / `[]byte`，忽略 Topic marshaler）补 pin 测试。

### 工程化

- workspace `go mod tidy`：清 `oklog/run` 残留间接依赖；
- 消除两处时序断言 flake（command Ready 等待下界、registry 订阅合并）。

**Full Changelog**: https://github.com/lynx-go/lynx/compare/v1.14.0...v1.15.0

## v1.14.0 (2026-09-23)

本次发布 tag：根 `v1.14.0`、`contrib/watermill/v1.7.0`、
`contrib/watermill-kafka/v1.8.0`、`contrib/cluster/v1.1.0`、
`contrib/cluster-redis/v1.1.0`、`contrib/registry/v1.9.0`、
`contrib/schedule/v1.9.0`、`contrib/consul/v1.8.0`。`contrib/telemetry`
与 `contrib/zap` 本批无源码变更，不重复打 tag。

本批为架构收敛批次（架构评审报告候选 1-8 全量落地）：eventbus 编解码/
重试/投递语义唯一归属、Bus 共享核心去重、关停流水线与 readiness 机制
收敛、cluster 租约引擎、config 单解码路径、Transport 投递模式契约。
基于 v1.13.0 rebase 集成：命令三级就绪（v1.12.0）与 D1-D4 竞态修复
（v1.13.0 前序）保留远端语义。

**应用迁移总览**（均为机械替换）：

- `PublishTyped` / `SubscribeTyped` / `PublishRawTyped` → `Topic.Publish` /
  `Topic.Subscribe` / `Topic.PublishRaw`；
- `WithSubscribeMarshaler`（订阅侧调用级覆盖，已删）→ 删除调用（订阅侧
  按 Topic / Bus 配置解码，有意不对称）；
- `bus.PublishRaw(ctx, topic, data)` → `bus.Publish(ctx, topic, data)`；
- `WithEnvForAllKeys()` → 删除调用（已是默认语义）；
- `ResolvePublishMarshaler` → `ResolveMarshaler`；
- 自定义 `eventbus.Transport` 实现：补 `DeliveryMode()` 一个方法（分区
  后端 `DeliveryConsumerGroup`、进程内广播 `DeliveryBroadcast`）。

### 破坏性变更：Transport 接缝声明投递模式与生命周期契约

- `Transport` 接口新增 `DeliveryMode() DeliveryMode`（`DeliveryBroadcast` /
  `DeliveryConsumerGroup`）：投递模式是每个后端必答的内在属性，Bus 的
  消费组占用检查（WK-01）据此启用，取代原"非内存 Transport 即检查"的
  `isMemoryTransport` 类型断言（`lynx.*` 强制内存后端的所有权判定仍用
  身份检查，属另一回事）。MemoryTransport → Broadcast，kafka →
  ConsumerGroup；自定义 Transport 实现需补一个方法；
- 新增可选能力 `eventbus.DefaultGrouper`（ConsumerGroup 后端）：暴露
  订阅键的配置默认组（kafka `consumer.group_id`）。Bus 据此计算有效组，
  **闭合 claimGroup 原已知局限一**——"handler 显式指定的组恰好等于另一
  handler 留空的默认组"此前 claim 键不同而静默放行（分区瓜分），现在
  两个方向（显式→默认、默认→显式）均拒绝；已知局限二（物理 topics
  重叠）保持文档化限制；
- `lynx.*` 强制内存后端的校验点 4 → 3（删除 Init 对 explicit 路由表的
  冗余再校验——该表只由 RouteKey 写入且写入前已校验）；
- `Transport` 接口文档写明生命周期归属契约（原 watermill 的 WK-10 注释
  升为接缝契约）：Transport 独立于 Bus 生存，需托管时实现 lynx.Service
  由应用 Register，Bus.Stop 只关自身与内置生命周期后端。

### 破坏性变更：删除 Bus.PublishRaw（等价面收敛）

- 两个实现均为一行委托 `Publish`，且 `Publish` 的 `[]byte` payload 分支
  本身就是原始字节直发（跳过序列化）——能力完全重叠，接口 9 方法收窄
  到 8。迁移：`bus.PublishRaw(ctx, topic, data)` → `bus.Publish(ctx,
  topic, data)`，逐字等价。`*RawEvent` 信封转发（保留 ID/Key/Headers/
  Time）不受影响——那是 `Topic.PublishRaw` 的职责。

### 破坏性变更：config 解码语义收敛——env 感知解码转正，双宇宙合一

- **转正**（应用确认在用）：结构体目标的 `Unmarshal`/`UnmarshalKey` 默认
  走结构体驱动逐叶取值（原 `WithEnvForAllKeys` 选项删除，迁移为删掉该
  选项调用）——tag 回退链 mapstructure → json → 小写字段名，仅在环境
  变量设置的键（配置文件无此键）参与解码；非结构体目标回落 viper 语义。
  `WithTagName` 收敛为单一含义（限定 tag，缺 tag 回退小写字段名）；
- 新增 `WithStrictTypes`：拒绝非字符串标量到集合的弱转（`brokers: 42`
  不再静默弱转为 `[]string{"42"}`）；字符串来源不受影响（env 值恒为
  字符串，`"a,b"` → 切表、`"42"` → int 均为合法弱转）。registry /
  watermill-kafka 的手写类型预检垫片（`validateRegistrySection` /
  `validateKafkaSection` + 逐字节重复的 `foldGet`/`isStringList`）删除，
  统一走本选项；
- 新增 `WithErrorUnused`（仅 `UnmarshalKey`）：报告配置子树中未被结构体
  消费的未知键（多为拼写错误）；remain/map 字段整体视为已消费；
- `registry.FileConfig` / `registry.LoadFileConfig` 成为 `registry.*` 段的
  唯一 schema：consul 删除自己 fileConfig 中手工同步的共享字段副本
  （enabled/backend/heartbeat_ttl/deregister_after），改经 LoadFileConfig
  读取（RC-06 的「解析即丢」假象消除）；
- 修复转正过程中发现的两处既有缺陷：`mapstructure:",remain"` 字段
  （kafka `map[逻辑topic]TopicOptions`）在结构体驱动路径下会静默解出
  空 map；结构体容器字段（map/切片且元素为结构体）的嵌套下划线键
  （如 `max_redeliveries`）按字段名匹配会静默解成零值——两处现均按
  mapstructure 语义解码并各有回归测试。

### 变更：cluster 租约引擎收敛——适配器骨架唯一化，TTL 下限进入契约

- 新增 `cluster/lease.go` 作为 Claim/Acquire 公共骨架的唯一归属：
  `ValidateCall`（ctx/名称/ttl 校验，三后端错误一致）、
  `RenewInterval`（ttl/3 续约间隔，1ms 下限）、`RunRenewLoop`（续约循环
  骨架，renew 报错即 cancel 租约 ctx 退出）；memory / cluster-redis /
  consul 三份手写骨架删除，适配器只留后端相关的存储/脚本/Session 逻辑；
- 新增可选能力 `cluster.TTLAware` / `cluster.MinTTL(c)`：适配器声明租约
  TTL 下限（Consul Session 10s；内存/Redis 无下限返回 0），不拓宽
  Coordinator 核心接口；
- **行为修复**：schedule 的 Exclusive 触发 TTL（间隔 + 1s）低于后端下限
  时自动钳制到下限并 Warn——此前短间隔任务配 Consul 会在每次触发时
  撞 `errTTLTooShort` 报错（该冲突原先只写在 contrib/consul 的注释里）；
  钳制只延长同一格子占位的存活期（格子名含时槽），不影响后续格子抢占
  （cron 与 Trigger 共用本路径）；
- 微小行为差异（多违规边界场景的报错优先级）：校验统一为
  ctx → 名称 → ttl 顺序，consul 的 ctx 检查从最后提前、redis 增加
  ctx 前置检查（错误值不变，仅提前）。

### 变更：eventbus 投递泵与克隆去重（Bus 共享核心收尾，内部重构为主）

- 新增 `watermill.PumpMessages`：watermill 消息 channel → `Delivery`
  channel 的共享投递泵（消息还原、逻辑 topic 回填、Ack/Nack 转达、下游
  停读防护 WK-05），MemoryTransport 与 watermill-kafka 共用，两份逐行
  相近的手写泵删除；
- 新增 `eventbus.CloneRawEvent`：内存 dispatch 与 Watermill 发布前的
  两份私有克隆合一（Headers 一律克隆为非 nil）；
- `SubscribeOptions.AutoAck` / `ContinueOnError` 补跨运行时语义文档
  （与重试/重投的互斥关系，两种 Bus 一致）；
- 新增双实现一致性测试：AutoAck / ContinueOnError / 重试预算同一场景
  矩阵在内存 Bus 与 Watermill(+MemoryTransport) 上断言相同调用次数；
  重试耗尽点为文档声明的有意分歧（内存丢弃 vs 持久化重投至重投上限），
  以测试钉死该分歧本身。

### 变更：readiness 等待机制收敛至 ready.go（内部重构，API 不变）

- 新建 `ready.go` 作为就绪等待的唯一归属：`awaitServiceReady`
  （Ready 通道 → Checker 轮询 → 直接放行）与 `awaitHealthy`
  （预算内轮询，可选 startErr 交错监听——peek 语义取出即放回）；
- `OrderedServices` 的 `waitReady`/`waitHealthy` 与 `newLynx` 的总线
  就绪轮询改为消费共享机制（预算仍由各自配置：前者组内默认 10s，
  后者 `BusReadyTimeout`）；错误文案与等待顺序逐字保留；
- 未纳入（有意）：OnPostStart 的 startWG 边界（非 readiness）、command
  的依赖等待（v1.12.0 已独立演进为三级就绪 + WithProbeTimeout，语义
  更丰富，保留）、drainChecker（关停信号，见 shutdown.go）。

### 变更：关停流水线收敛至 shutdown.go（内部重构，API 不变）

- 新建 `shutdown.go` 作为关停流水线的唯一归属：`drain.go`（`ErrDraining` /
  `drainChecker`）并入删除，`stopServiceBounded` / `stopServices` /
  `hasDrainHooks` / `runOnDrainHooks` / `runOnPreStopHooks` /
  `runPostStopHooks` 从 `lynx.go`（1015 行）迁入；
- 逐行近似的 `runOnDrainHooks` / `runOnPreStopHooks` 合并为参数化的
  `runHooksInBudget`（阶段文案差异收进 `shutdownPhase`，日志与错误字符串
  逐字保留）；
- 手写 bounded-wait 惯用法从七处收敛为 `callBounded` 原语（结构性超时
  判定，不用错误值区分预算耗尽与 fn 自身错误）：服务停止、三阶段钩子
  改用；命令侧保留 v1.12.0 的三级就绪实现（`checkHealthBounded`/
  `waitReadyBounded`，语义更丰富）；
- 行为零变更：阶段顺序、预算、错误聚合、日志/错误文案均逐字保留
  （全量 `-race` 回归通过）。

### 破坏性变更（第一至三批，随本版本合并发布）：eventbus 编解码收敛与 Bus 共享核心

- 编解码解析唯一归属 `ResolveMarshaler`（原 `ResolvePublishMarshaler`/
  `ResolveSubscribeMarshaler`）；删除 `WithSubscribeMarshaler` 与
  `SubscribeOptions.Marshaler`（订阅侧无调用级覆盖，有意不对称）；
  删除 `PublishTyped`/`SubscribeTyped`/`PublishRawTyped` 迁移别名；
- `MarshalerFor` 接通 `Topics[t].Marshaler`（查找序 `TopicMarshalers[t]`
  → `Topics[t].Marshaler` → 全局 → JSON）；新增 `SubscribeOptions.Retry`
  与 `WithSubscribeRetry`，四级重试合并接通（§10.4 承诺兑现）；
- 发布侧组装收敛为 `eventbus.BuildRawEvent`、订阅默认合并收敛为
  `eventbus.ApplyTopicConfig`、`Options` 增 `PropagateKeys`/`LogMessageFor`/
  `RetryFor`：memory 与 watermill 的成对手写实现全部删除，协议键清除
  漂移（watermill 硬编码三键未走 `isProtocolMetaKey`）随之消除；
  watermill wire 转换导出 `ToMessage`/`FromMessage`，kafka 删除逐字节
  相同的自有副本（§5.1 单一映射点成真），watermill-kafka 为此新增对
  `contrib/watermill` 的模块依赖。

## v1.13.0 (2026-09-23)

本次发布 tag：根 `v1.13.0`、`contrib/schedule/v1.8.0`（Trigger/WithNow）、
`contrib/telemetry/v1.7.0`（runtime metrics）、`contrib/registry/v1.8.0`
（Subscribe）。其余 contrib 无源码变更，不重复打 tag。本批为 ROADMAP
Phase G「运行时可调性与可观测」主线 + 若干存量承诺回收，API 只增不改。

### 新增：运行时日志级别调整与 `/version` 构建信息（debug 服务）

- `lynx` 新增 `SetLogLevel`/`LogLevel`（`*lynx` 方法，不动 App 接口）：
  配置过 `log-level` 时直接修改既有 LevelVar 即时生效；未配置时经
  `slog.SetLogLoggerLevel` 兜底；用户 `SetLogger` 定制 handler 后拒绝
  代理（返回 false）
- debug 服务新增 `GET/POST /loglevel`（query 或 JSON body 调整；非法
  级别 400、无控制能力 501、定制 logger 409）与 `/version`
  （`-ldflags -X` 注入 `debug.BuildVersion/BuildCommit/BuildDate` +
  Go/OS/Arch + 应用元数据，注入示例见 `debug/vars.go`）

### 新增：配置热更新 `WithConfigWatch`

显式启用：文件变更经 viper 自动重读（后续 `Config()` 读取返回新值），
框架向总线发布 `lynx.config.updated` 事件（`eventbus.ConfigUpdatedTopic`），
订阅方自行决定响应粒度。`lynx.*` 前缀在跨进程 Bus 上强制内存路由，
热更新事件不出进程；无文件来源（`WithConfig` 注入/未指定 `--config`）时
`Run()` 启动期快失败（镜像 poison-pill 语义）。已知取舍（注释化）：viper
watcher 无撤销 API，随进程常驻。

### 新增：Go runtime metrics 开箱接入（telemetry）

otel runtime instrument（goroutine/GC/内存）默认注册到 MeterProvider，
随指标管线输出，零配置获得进程级可观测基线；`WithoutRuntimeMetrics()`
关闭。依赖钉 v0.67.0 与既有 otel contrib 版本线对齐。容器 CPU 配额感知
（GOMAXPROCS 修正）由 Go 1.25+ runtime 内建，不引入 automaxprocs。

### 新增：出站熔断 `WithCircuitBreaker`（client/http）

封装 sony/gobreaker/v2（TwoStepCircuitBreaker），公共 API 零 gobreaker
类型（`CircuitBreakerOptions` 自有配置面：连续失败阈值/半开放行数/open
时长/失败状态码/状态回调）。集成于 `Do` 最外层、重试之外：一次调用整体
成败只计一次；熔断打开时本地快速拒绝（`errors.Is(err, ErrCircuitOpen)`）
不进重试循环。失败判定默认仅传输层错误，`FailureStatusCodes` 按需扩展；
客户端取消/超时经 `IsExcluded` 排除（不误开熔断）；状态迁移记 Info 日志
并转交回调。选型结论：经典三态为业界收敛点，failsafe-go 与既有 backoff
重试职责重叠故排除。

### 新增：`Resolver.Subscribe` 消费侧订阅（registry）

与 `Discovery.Watcher` 同构的缓存层订阅：首个 `Next` 立即返回当前快照、
信号合并（慢消费者只拿最新）、`Stop` 广播唤醒阻塞中的 `Next`、
`Stop` 后 `ErrWatcherStopped` / Resolver 关闭后 `ErrResolverClosed`；
订阅者对后端形态无感（DNS 式轮询后端同样触发）。gRPC resolver 改订阅
驱动：实例变化毫秒级反映到 `UpdateState`（原 5 秒轮询消除），订阅终止
退回 30 秒兜底轮询；`ResolveNow` 与去重/空快照/保态语义不变。
设计文档见 `docs/design-resolver-subscribe.md`。

### 新增：schedule `Trigger` 手动触发与 `WithNow` 时钟注入

`Scheduler.Trigger(name)` 立即执行任务，与 cron fire 共用同一路径（panic
恢复、Exclusive TryOnce 互斥、错误上报），未注册返回 `ErrTaskNotFound`：
测试无需真等 cron 时序，生产兼作手动执行运维口。`WithNow` 注入互斥格子
计算时间源，使分布式互斥行为可确定性断言。

### 改进：v1.1.0 承诺回收——服务器级 ErrorHandler 与按维度限流

- `WithErrorHandler(h)` + `Server.NewErrorHandler(h, fn)`：h 传 nil 的
  兜底依次取服务器级默认 → 包级 `DefaultErrorHandler`；典型用法
  `WithErrorHandler(DefaultErrorHandlerWithLogger(srvLogger))` 接入
  服务日志。包级 `NewErrorHandler` 行为不变
- `RateLimitPerKey(rps, key)` 按 key 分桶限流，预置路由/客户端 IP/
  user_id 三个提取器，自定义提取器可组合维度；桶存储带惰性清扫
  （每 1024 请求扫除 10 分钟空闲桶）；空 key 退化为共享桶；可与
  服务器级 `RateLimit` 叠放

### 改进：gRPC 服务端 request_id/user_id 还原闭环

`interceptor.RequestIDPropagation()`/`...Stream()` 从 incoming metadata
还原为日志属性（校验与 HTTP 侧 SC-22 同规：≤128、`[A-Za-z0-9-_]`，非法
丢弃防日志污染）；默认装配于 Recovery 之后、请求日志之前，请求日志自动
携带 id，下游调用继续透传——client 写入 + server 还原开箱闭环。新增
`grpc.RequestIDFrom(ctx)` 与 HTTP 侧对称。

### 改进：命令依赖探测上界可配（`WithProbeTimeout`）

`healthCheckTimeout`/`readyWaitTimeout` 收敛为 `defaultProbeTimeout`
默认值，`WithProbeTimeout` 统一覆盖，非法值回落默认。

### 工程与文档

- CI 新增 govulncheck 依赖漏洞扫描（vuln job + `mise run vuln`，工具钉
  v1.8.0）；首跑即发现 go1.26.5 标准库 6 个可达漏洞，工具链钉版升
  1.26.8（`go.mod` 语言指令保持 1.26.5 不变）
- 9 个 contrib 模块补齐 README（符号/配置键/file:line 经源码核对）；
  `_examples/bus` 补 README；`contrib/watermill` 补 LICENSE
- 新增 CONTRIBUTING.md、SECURITY.md、issue 模板（开源协作基建）；
  docs/01 新增「范围边界」小节（数据层不做等）；docs/05 写明 gRPC
  reflection 常开的取舍
- 修复 `_examples/cli` errcheck 失败（83b0307 引入，lint(_examples)
  CI 自该提交起红灯）
- ROADMAP 三路独立复核修正（版本归属对账、失联承诺回收、Phase G
  扩充至 20 项并重排），见 `ROADMAP.md`

## v1.12.0 (2026-09-22)

本次发布 tag：根 `v1.12.0`、`contrib/watermill-kafka/v1.7.0`（Transport
补 `Ready`）。其余 contrib 无源码变更，不重复打 tag。

### 改进：命令依赖等待切换三级就绪解析，kafka Transport 补 `Ready`

命令（`app.Command`）执行前的依赖等待从"轮询 HealthCheckers"改为与
`OrderedServices` 启动排序相同的三级优先：实现 `lynx.Ready` 的服务等待
channel 关闭（边沿信号：单调、失败不关闭）；否则实现 `Checker` 的有界
轮询（既有行为，单次 3s 上界不变）；两者皆无视为随启动即就绪。

语义修正：

- 依赖 `Start` 失败时命令经 run group 中断**即时**退出并报
  `aborted waiting for dependencies`——此前要烧完 MaxTries 预算后误报
  "timed out waiting"（真实错误在失败方）；
- Ready 等待无轮询延迟、无 `checkHealthBounded` 式挂死取舍；已就绪的
  信号即使 ctx 已取消也放行（保持"首查即就绪即运行"语义）；
- `MaxTries`/`WithBackoff` 保持轮次预算语义；外部 `AppContext` 实现回
  退健康检查聚合（既有行为）；错误信息从 "to be healthy" 改为
  "to become ready"。

配套：`contrib/watermill-kafka` Transport 补 `Ready()`（Start 置位运行
标志后关闭，与 CheckHealth 翻转时刻一致；连接仍惰性建立）；http/grpc/
debug 服务已有 Ready，无需改动。

### 新增：`lynx.WithBusProvider`——配置驱动的总线构造

总线依赖配置（`bus:`/`kafka:` 段）时，此前必须在 `NewRunner` 之前自行
读取配置再经 `WithBus` 注入（该约束此前未在任何文档中写明）。新增
`WithBusProvider(fn)`：框架在构造序列内、配置装配完成后以装配好的
`lynx.Config` 调用 fn 构造总线，与 `WithBus` 注入走完全相同的后续链路
（Init → 提前 Start → BusReadyTimeout 就绪等待 → SetDefault → ctx 内嵌
→ 生命周期事件 → Run 收尾 Stop）：

```go
lynx.NewRunner(setup,
    lynx.WithBusProvider(func(cfg lynx.Config) (eventbus.Bus, []lynx.Service, error) {
        kt, err := wmkafka.NewFromConfig(cfg)
        if err != nil {
            return nil, nil, err
        }
        bus, err := watermill.NewFromConfig(cfg, transportsOf(kt))
        return bus, servicesOf(kt), err // Transport 生命周期由框架托管
    }),
)
```

- 返回的 `[]Service`（如 kafka Transport）按 `Register` 语义注册：Init
  同步执行、Start/Stop 纳入生命周期、实现 `Checker` 的进入健康聚合
  （CLI 命令的健康等待因此能等 Transport 就绪）；
- 优先级：显式 `WithBus` > provider > `WithBusOptions`/默认内存总线，
  且与 `WithBusOptions` 的先后顺序无关（provider 不会被静默击败）；
- provider 错误或返回 nil 总线视为构造失败，经 `RunE`/`NewApp` 返回。

### 新增：`lynx.WithConfigFile`——CLI 配置桥接

子命令式 CLI（如 lynx-go/commands）的参数已由外部解析，此前需要手工
组合 `WithDisableConfigFlags()` + `WithBindConfigFunc(...)` 把解析出的
配置路径接进框架——两选项顺序敏感（写反会静默丢失绑定）。新增单一
选项 `WithConfigFile(path)`：关闭框架内置的 os.Args 解析、路径直接
绑定配置文件，空路径回退搜索工作目录（与 `DefaultBindConfigFunc`
一致）。`_examples/cli` 已改用该写法。测试场景的分层配置注入见
`lynxtest`（`WithConfigBaseline` 系列）。

### 更名：`lynxtest.WithConfigFile` → `WithConfigBaseline`

避免与新增的 `lynx.WithConfigFile`（生产入口：声明配置路径并关闭默认
flags）同名混淆。语义不变：测试侧构造期即时读入文件作为分层基线
（基线 → `WithConfigYAML` → `WithConfigMap`）。属未发布 API 更名，
无迁移成本。

### 修复：启动期关停交错竞态族（D1-D4）

oklog/run 的 interrupt 可先于服务 actor 的 execute 执行，`Close` 可先于
后台 `Run` goroutine 调度到达——四类实证缺口（详见
`docs/design-startup-race.md`）：

- **D1**：首个服务 Start 快速失败触发中断时，兄弟监听服务的 Stop 可能
  先于其 Start 执行，Start 仍会执行并永久 Serve、Run 挂死。修复：服务
  actor 的 execute 闭包在 Start 前检查中断（已中断不再 Start）；
- **D2**：`Close` 先于 `Run` 调度执行时，Run 仍完整执行僵尸生命周期
  （订阅已关闭总线报错等）。修复：新增 `closed` 状态与哨兵错误
  `lynx.ErrAppClosed`——Close 持锁置位（幂等），Run 入口同域检查直接
  返回；
- **D3**：HTTP server 在 Stop-wins 交错下 Start 进入永久 Serve。修复：
  新增 `stopRequested` 标志（Stop 先置位再读 httpServer；Start 在 Serve
  前检查，已中断则关闭监听器返回 nil）；
- **D4**：gRPC server 在 Stop-wins 交错下 `Serve` 返回
  `grpc.ErrServerStopped` 未归一化，正常关停被误报为服务失败。修复：
  归一化分支补识别该哨兵错误（仅 `stopRequested` 已置位时）。

生产影响：启动期收到 SIGTERM 的进程此前可能无法退出（D3）或误发
`lynx.service.failed` 虚假事件（D4）。`lynxtest` 的 cleanup 将
`ErrAppClosed` 视为合法交错不报失败。

### 新增：`lynxtest` 测试套件

- `lynxtest.Run`：L2 组装测试——以与生产 main 相同的 Setup 在测试进程内
  拉起完整应用；配置分层注入（`WithConfigBaseline` 基线 → `WithConfigYAML` →
  `WithConfigMap` 覆盖，裸调用注入空配置，不读 os.Args/工作目录）、快速
  超时基线、`t.Cleanup` 走生产同源关停序列并恢复进程级全局；
  `WithTBLogger()` 可把应用日志接到测试输出；
- 句柄 `App.WaitReady/Exited/Err`：应用先于就绪退出时立即带出 `Run`
  实际错误；
- 拨号辅助：`HTTPClient`/`GRPCConn`（直连回环、绕过代理环境变量，就绪
  预算与 gRPC 拨号选项可调）与 `BufconnHTTPClient`/`BufconnGRPCConn`
  （配合 `WithListener` 注入 bufconn，免 TCP 端口）；
- `NewContext`：L1 服务单元测试的可用 `AppContext`（真内存总线 + 注入
  配置 + 接测试日志），配套 `ContextWithConfig/ConfigMap/ConfigYAML/Bus/
  Logger/BusReadyTimeout`；
- 配套：`docs/08-testing.md` 使用章节、`_examples/testing` 可运行示例。

### 框架侧可测性 API（增量）

- `lynx.NewApp(opts...)`：不经 Runner 直接构造 App，opts 应用顺序与
  `NewRunner` 严格一致；
- `lynx.WithConfig(cfg)`：注入 `Config` 实例，构造期跳过 flags/文件装配；
- `lynx.WithIsolated()`：不触碰进程级全局（`lynx.Set`/
  `eventbus.SetDefault`/`slog.SetDefault`），同进程多 App 场景；
- `lynx.Server` 接口：`Service` + `Addr/AdvertiseAddr/Ready`，
  `server/http`、`server/grpc` 均实现；
- `http.WithListener(net.Listener)` / `grpc.WithListener(net.Listener)`：
  注入监听器；gRPC 关停归一化顺带识别 bufconn 的裸 `"closed"` 错误。

### 工程化

- CI Test 步骤加 `-shuffle=on`；`mise.toml` 新增 `test`/`test-integration`
  任务（逐 workspace 模块遍历，与 CI 同参数）。

### 变更：Bus 语义下沉共享核心（memory / watermill 去重）

- 发布侧 RawEvent 组装收敛为 `eventbus.BuildRawEvent`：payload 类型分派
  （`*RawEvent` 透传 / `[]byte` / `nil` / 类型化经 Marshaler）、协议键清除、
  Metadata 合并、日志属性白名单传播、ID/Time 默认——两个 Bus 实现此前各写
  一份，且协议键清除已出现硬编码漂移（watermill 侧未走 `isProtocolMetaKey`）；
- `eventbus.Options` 新增 `PropagateKeys` / `LogMessageFor` / `RetryFor`，
  propagate / log / retry 的解析从各 Bus 私有函数收敛到 Options 一处；
- 订阅的 `Topics[t]` 默认合并收敛为 `eventbus.ApplyTopicConfig`；
- watermill 的 wire 转换导出为 `watermill.ToMessage` / `FromMessage`，
  watermill-kafka 删除逐字节相同的自有副本改为复用（§5.1 单一映射点成真）；
  watermill-kafka 为此新增对 `contrib/watermill` 的模块依赖；
- watermill 行为微调：`*RawEvent` 透传时空 ID 现在回退生成 UUID（与内存
  Bus 对齐，此前保持空串）。

## v1.11.0 (2026-09-15)

本次发布 tag：根 `v1.11.0`、`contrib/registry/v1.7.0`（两处 `Bind`
更名 `Apply`）。其余 contrib 无源码变更，不重复打 tag。

### 破坏性变更：`boot.Bind` / `registry.Bind` 更名 `Apply`

「把聚合好的钩子/服务注册进应用」的两个入口统一从 `Bind` 更名为
`Apply`：

- 根模块：`(*boot.Bootstrap).Bind(app)` → `(*boot.Bootstrap).Apply(app)`；
- `contrib/registry`：`registry.Bind(app, r)` → `registry.Apply(app, r)`
  （推荐入口：Register 服务 + 挂 OnDrain 注销钩子；nil no-op 语义不变）。

动机：`Bind` 在同一生态三重撞名——google/wire 的 `wire.Bind`（接口
绑定实现，而 `boot` 包正是 Wire 引导）、lynx 配置域的
`BindEnv`/`BindPFlags`（数据绑定），且 `b.Bind(app)` 读作「把 app 绑到
b 上」，方向与实际语义相反；`Apply` 主宾方向明确、无撞名。无兼容
别名，迁移为机械重命名（示例与文档已同步）。

## v1.10.0 (2026-09-15)

本次发布 tag：根 `v1.10.0`、`contrib/zap/v1.7.0`（破坏性改名
`SyncOnStop` → `SyncOnPreStop`）。`boot` 包随根模块发布；
`contrib/registry` 仅测试文件适配新接口，无源码变更，不重复打 tag。
内部消费方（torchwood）随发版迁移，无兼容别名。

### 依赖

- **grpc** `v1.83.0` → `v1.83.2`（根、`contrib/registry`、`contrib/consul`、
  `_examples`）：修复 CVE-2026-84303（xDS RBAC HTTP Filter 混合大小写
  头匹配绕过，medium）、CVE-2026-84304（HTTP/2 DATA 分片导致堆内存
  耗尽，high）、CVE-2026-84445（xDS server 缺失 `:authority`/`Host`
  头导致崩溃 DoS，high）。连带 `golang.org/x/net`、`x/text` 小版本。

### 破坏性变更：`DrainHookTimeout` 并入 `DrainTimeout`

OnDrain 钩子的独立预算 `DrainHookTimeout`（默认 3s）移除：**排水窗口
`DrainTimeout` 即钩子总预算**——钩子与窗口睡眠并发执行，窗口结束时
未完成的钩子记超时错误并继续关停。关停上界公式从
`max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout + Σ StopTimeout`
简化为 `DrainTimeout + ShutdownTimeout + Σ StopTimeout + CleanupTimeout`
（钩子与睡眠并发，不叠加）。

`DrainTimeout=0` 语义收紧为**整段禁用**（窗口与钩子）：此时注册了
OnDrain 钩子属于配置错误，`Run()` 启动期返回新哨兵
`ErrDrainHooksRequireDrainTimeout` 并逆序停止已 Init 的服务——快失败
好过关停期静默跳过注销的延迟暴露。旧行为（`DrainTimeout=0` 时钩子仍以
3s 预算执行）不再保留；迁移：依赖 OnDrain 钩子的应用（如
`registry.Bind`）显式设置 `WithDrainTimeout`（Registrar 注销 RPC 自身
有 3s 内部上界，窗口 ≥3s 即可覆盖）。

移除符号：`Options.DrainHookTimeout`、`WithDrainHookTimeout`、
`DefaultDrainHookTimeout`、`ErrDrainHookTimeoutInvalid`。

### 破坏性变更：生命周期钩子补齐为五阶段并按 Pre/Post 命名

钩子阶段补齐为 `OnPreStart → OnPostStart → OnDrain → OnPreStop → OnPostStop`
（Pre/Post 锚定**服务组**的启停），`OnStart`/`OnStop` 更名，并新增两个阶段：

| 旧（≤ v1.9.0） | 新（v1.10.0） | 触发时机 | 签名 / 预算 |
|---|---|---|---|
| `app.OnStart` | `app.OnPreStart` | 服务启动前，串行，首错中止启动 | `HookFunc` / 无 |
| —（新增） | `app.OnPostStart` | 所有服务 actor 进入执行体（`Start` 调用紧随其后；阻塞型服务以 actor 进入为界，**非就绪语义**），串行，与运行中的应用并发；钩子错误触发关停 | `HookFunc` / 无 |
| `app.OnDrain` | `app.OnDrain`（不变，预算语义变更） | 排水窗口内与 `DrainTimeout` 睡眠并发，**窗口即钩子总预算** | `HookFunc` / `DrainTimeout`（见下方合并说明） |
| `app.OnStop` | `app.OnPreStop` | 服务 Stop 之前（仍在服务在途请求） | `HookFunc` / `ShutdownTimeout`（默认 5s） |
| —（新增） | `app.OnPostStop` | 所有服务与总线停止之后、`Run()` 返回前，逆序（LIFO）；覆盖 `Run()` 全部退出路径（含服务 Init 失败、`OnPreStart` 失败），`Run()` 未调用时由 `Close()` 兜底，恰好执行一次 | `CleanupFunc`（`func()`）/ `CleanupTimeout`（默认 10s） |

`OnPostStop` 的动机：Wire injector 返回的 `cleanup`（关闭 DB/Redis 连接池等
DI 底层资源）此前没有正确归宿——放 `OnPreStop` 会在服务还在服务时就关掉
连接池（排水/关停期间在途请求失败），消费方只能在 `RunE()` 返回后手写
goroutine + 超时兜底样板。现在 `app.OnPostStop(cleanup)` 一行接入（签名与
Wire 生成的 `func()` 原生对齐），超时跳过、逆序、全路径覆盖由框架保证。

同批更名（无兼容别名）：

- `lynx.CleanupFunc` 新类型（`func()`）。
- `Options.CleanupTimeout` / `WithCleanupTimeout`（0 = 默认 10s，负值
  `Validate` 报 `ErrCleanupTimeoutInvalid`）。
- `boot.OnStartHooks` → `boot.PreStartHooks`，`boot.OnStopHooks` →
  `boot.PreStopHooks`，新增 `boot.DrainHooks`（原 `WithDrainHooks` setter
  折叠进 `New`）与 `boot.PostStopHooks`。`boot.New` 参数顺序修正为与字段
  声明一致：`New(preStarts, drains, preStops, postStops, services,
  serviceFactories)`——历史上 drains 因 Wire injector 兼容被挤成 setter，
  本次为不兼容版本，顺带修正。**全部 Wire injector 需重新生成。**
- `contrib/zap.SyncOnStop` → `SyncOnPreStop`。
- `Runner.RunE` 在 setup 失败时调用 `app.Close()` 释放应用（兜底执行
  `OnPostStop` 钩子、停止提前启动的总线），与"cleanup 赋值后无论成败都
  执行"的手写语义对齐。
- 内部错误信息随更名：`"on-stop hook timed out"` →
  `"on-pre-stop hook timed out"` 等。

### 迁移对照

```go
// v1.9.0                          // v1.10.0
app.OnStart(hook)                  app.OnPreStart(hook)
app.OnStop(hook)                   app.OnPreStop(hook)
// wire cleanup 原手写超时样板：    app.OnPostStop(cleanup)
boot.New(starts, stops, svcs, f)   boot.New(preStarts, drains, preStops, postStops, svcs, f)
zap.SyncOnStop(l)                  zap.SyncOnPreStop(l)
```

## v1.9.0 (2026-09-13)

本次发布 tag：根 `v1.9.0`（唯一变更模块，contrib 无改动不重复打 tag）。

### 破坏性变更：`Config.Unmarshal` / `UnmarshalKey` 增加 `UnmarshalOption` 变参

签名变更为 `Unmarshal(out any, opts ...UnmarshalOption) error` 与
`UnmarshalKey(path string, out any, opts ...UnmarshalOption) error`：调用方
不传 opts 时源码兼容、行为零变化；自带 `Config`/`ConfigSource` 实现的外部
代码需同步签名。`UnmarshalOptions`（`TagName` / `EnvForAllKeys`）为解码器
无关概念，任意实现应可解释：

- `WithTagName(tag)`：指定匹配配置键的 struct tag（默认路径 viper 语义，
  仅 mapstructure）。
- `WithEnvForAllKeys()`：结构体驱动的逐叶取值——以目标结构体叶子为键集
  逐键 `Get`，使仅在环境变量中设置的键（配置文件无此键）也参与解码；
  viper `Unmarshal` 基于 `AllSettings`，对此类键不可见。叶子键按
  `mapstructure → json → 小写字段名` 回退（`TagName` 显式设置时仅该 tag
  回退字段名），解码语义对齐 viper 默认（`WeaklyTypedInput` +
  duration/逗号切分钩子）；非结构体目标与动态键 map 字段回落 viper 路径。

## v1.8.0 (2026-09-13)

本次发布 tag：根 `v1.8.0`（唯一变更模块，contrib 无改动不重复打 tag）。

### 新增

- **核心**：`ConfigSource` 新增 `SetEnvKeyReplacer(*strings.Replacer)`，透传 viper
  同名能力（默认实现适配），恢复 v1.0.0 精简接口时移除的「环境变量键名映射」——
  设置前缀与 replacer（如 `"." → "_"`）+ `AutomaticEnv` 后任意点分键都能被
  `PREFIX_A_B` 形式的环境变量覆盖，调用方不再需要逐键 `BindEnv` 变通。对自带
  `ConfigSource` 实现的外部代码为接口增量，需补一个透传方法。

### 修复

- **docs**：修正 `02-quick-start.md` 中「`Unmarshal` 按结构体 tag（`mapstructure`
  或 `json`）解码」的错误表述——默认实现（viper 适配）仅按 `mapstructure` tag
  或字段名大小写不敏感匹配，`json` tag 不参与匹配，snake_case 配置键（如
  `access_key_id`）无法直接解码到 CamelCase 字段。

## v1.7.0 (2026-08-27)

`cluster.Store` 更名为 `cluster.Coordinator`。本次发布 tag：根 `v1.7.0`（仅文档）、
`contrib/cluster/v1.0.0` 与 `contrib/cluster-redis/v1.0.0`（首次发布）、
`contrib/consul/v1.7.0` 与 `contrib/schedule/v1.7.0`（相对 v1.6.0 为破坏性变更）。

### 破坏性变更：`cluster.Store` 更名为 `cluster.Coordinator`

`Store` 名字暗示持久化存储，实际是进程间协调端口（`Claim` 一次性占位、
`Acquire` 长租约）。更名为 `Coordinator` 与包语义（「进程间协调」「协调后端」）
一致，不留兼容别名。受影响模块：`contrib/cluster`、`contrib/cluster-redis`、
`contrib/consul`、`contrib/schedule`。迁移对照：

| 旧 | 新 |
|---|---|
| `cluster.Store` | `cluster.Coordinator` |
| `cluster.ErrNilStore` | `cluster.ErrNilCoordinator` |
| `clusterredis.NewStore` | `clusterredis.NewCoordinator` |
| `consul.Client.Store()` / `consul.NewStore` | `consul.Client.Coordinator()` / `consul.NewCoordinator` |
| `schedule.Options.Store` / `schedule.WithStore` | `schedule.Options.Coordinator` / `schedule.WithCoordinator` |
| `schedule.ErrStoreRequired` | `schedule.ErrCoordinatorRequired` |

`Claim` / `Acquire` / `Lease` / `Leadership` / `TryOnce` / `Campaign` /
`Singleton` / `NewMemory` 等方法与配方签名语义不变，仅接口参数类型随更名。

### 修复

- **`contrib/consul` go.mod 版本引用修正**：`require` 的
  `contrib/registry` 从不存在的 `v1.0.0` 修正为已发布的 `v1.6.0`
  （此前被本地 `replace` 遮蔽，模块代理无法解析；v1.6.0 tag 即带此问题）。

## v1.6.0 (2026-08-25)

全量架构与代码审查的修复版本：83 项发现全部处置（81 修复 + 2 项约定不修），
逐项明细、复审记录与遗留低危项见 `docs/review-2026-08-25.md`。相对 v1.5.2
API 保持向后兼容（全部为增量），10 模块 `go vet` + `go test -race` 全绿。

### 破坏性/行为变更（修复目标，升级注意）

- **`client/http` 超时语义修正**：`Do` 不再在返回时取消 ctx——取消时机绑定到
  响应体 `Close()`/EOF（仿标准库 `cancelTimerBody`）。此前默认 30s 超时会让
  大响应/分块/流式 body 读取必得 `context canceled`（Critical）。
- **5xx 错误信息不再回传客户端**：HTTP `DefaultErrorHandler` 对 5xx 返回通用
  消息（`http.StatusText`），gRPC Recovery 对外只返回通用 "internal error"；
  错误详情与 panic 堆栈仅进日志（信息泄露修复）。
- **正常关停不再误发 `lynx.service.failed`**：HTTP `ErrServerClosed` 与 gRPC
  关停期的 closed-connection 错误在 `Start` 内归一化为 nil。
- **`contrib/zap` 级别域统一为 slog 域**：`fatal`/`info+2` 等输入从合法变报错
  （框架默认路径不受影响）；同时禁用 zap 生产默认采样（此前同级别日志
  100 条后每 100 条只记 1 条，错误日志静默丢失）并禁用非标准 slog 级别被
  降级为 Info 的映射（clamp 到最近标准级）。
- **HTTP server 关停 deadline 取 min**：调用方 Context deadline 与
  `ShutdownTimeout` 并存时取较早者（与 gRPC 侧对齐；`ShutdownTimeout=0`
  仍为显式无上界）。

### 新增

- **核心**：`lynx.WithBusReadyTimeout`（默认 10s）替换总线就绪的 1 秒硬编码；
  Command 依赖等待的单次健康检查限时 3s（阻塞型 checker 不再挂死等待循环）。
- **server/http**：`WithHealthCheckTimeout`（默认 3s，检查器并发执行+单查限时+
  panic 兜底）、`WithHealthCheckPrefix`、`WithDisableHealthCheck`、
  `DefaultErrorHandlerWithLogger`。
- **server/grpc**：`WithShutdownTimeout`（`WithTimeout` 的推荐别名）、
  `WithRequestLog(bool)`（默认开，可关）、`WithRequestLogLevel(slog.Level)`、
  `RecoveryWithLogger`/`RecoveryStreamWithLogger`（记录 panic 值+堆栈）。
- **client/http**：`Retry-After` 等待上限 min(Retry-After, 剩余超时, 2min)，
  覆盖全部剩余预算时不再发起注定超时的重试；非幂等重试警示入文档。
- **contrib/watermill**：毒消息重投上限 `bus.max_redeliveries`（默认 10，
  主题级/`WithMaxRedeliveries` 可覆盖），超限记 Error 并 Ack 丢弃；非内存
  Transport 上同 topic 多 handler 共用消费组被 `Subscribe` 拒绝（Kafka 组内
  瓜分=静默半量丢消息），广播请用不同 group、竞争消费用单 handler+instances。
- **contrib/registry**：`MatchFilter` 导出（consul 侧副本删除）、
  `Status.String()`/`MarshalJSON`/`UnmarshalJSON`（字符串形式，兼容数字）、
  `WithResolverLogger`；`heartbeat_interval ≥ heartbeat_ttl` 构造期报错。

### 修复（按模块择要，完整清单见 review 文档）

- **核心/eventbus**：memoryBus `Publish`/`Stop` 竞态 send-on-closed-channel
  panic（发送移入读锁临界区）；`Topic.Subscribe` 每消息重复解析 Marshaler；
  `Runner.RunE` 无锁读；内存 Bus at-most-once 语义文档化。
- **server/client**：健康检查全链路（readiness 端点、gRPC health 轮询）加
  超时与并发（此前阻塞型 checker 挂起探测、冻结轮询状态并泄漏 goroutine）；
  `WithTimeout` 双语义对齐；healthz 端点 Recovery 兜底与路径可配；requestlog
  去掉每请求深拷贝与死回调；`Start` 重入守卫；`X-Request-Id` 入站校验。
- **contrib/watermill-kafka**：关闭链路三处裸 channel 发送加 ctx 保护
  （goroutine 泄漏+in-flight 确认丢失）；Init 预构建并 `Validate()` sarama
  配置（非法 SASL/压缩/offset 启动期报错）；Stop/Publish 竞态复查；
  同集群配置差异指纹比对 Warn（一次性）；instances 上限 64。
- **contrib/consul**：`Register` 传入 ctx（`ServiceRegisterOpts.WithContext`，
  此前 3s 预算完全失效、Agent 不可达可无限挂起）；blocking query index 回退
  sanity check（Raft index 回绕后 watch 永久失效）；`Node.Address` 回落
  （裸 `:port` Endpoint 契约三处对齐）；零权重规格化。
- **contrib/registry**：Resolver 关闭时 Stop 后端 watcher（条目泄漏）；
  gRPC resolver `GetAll` 带超时且无变化不 `UpdateState`；Watch/Close 注册
  竞态窗口；DNS 双路径 Name 统一 FQDN。
- **辅助模块**：telemetry 并发 Init CAS 化；schedule `WithLogger` 不再被
  Init 覆盖、Stop 等待在途任务（保留 `cron.Stop()` 句柄）、`WithLocation`
  对自定义 cron 实例 Warn；debug 独立使用时 cancel 释放端口；logging 批次内
  重复 key 去重；zap Sync 放行良性 errno。

---

## v1.5.2 (2026-08-25)

### 破坏性变更

- **Subscribe `handlerName` → Option**：`Bus.Subscribe` / `Topic.Subscribe` /
  `SubscribeTyped` 不再接收位置参数 `handlerName`；改用
  `eventbus.WithHandlerName`，省略时默认为 topic 名（同 topic 多订阅者需显式命名）。

### 变更

- **Topic API 整理**：`Topic[T]` 方法集中到 `topic.go`；`Options()` 返回公开类型
  `TopicOptions`。

---

## v1.5.1 (2026-08-24)

### 新增

- **核心—全局 AppContext**：`lynx.Set` / `lynx.Get` 提供进程默认
  `AppContext`（类比 `eventbus.SetDefault` / `slog.SetDefault`）；
  `newLynx` 成功后自动 `Set(app)`，测试可用 `Set(nil)` 清理。

---

## v1.5.0 (2026-08-24)

EventBus 一等化：统一 `Bus` / `Topic[T]` / `Event[T]`，删除 `contrib/pubsub`，
`contrib/kafka` 重命名为 `contrib/watermill-kafka`。设计见
`docs/design-eventbus.md`。

### 破坏性变更

- **移除 `contrib/pubsub`**：进程内/跨进程消息统一走核心 `eventbus`（`Bus` /
  `Topic[T]` / `Event[T]`）与 `contrib/watermill` Bus 实现。
- **`contrib/kafka` → `contrib/watermill-kafka`**：模块路径重命名；实现
  `eventbus.Transport`（不再依赖 pubsub）。配置段仍为 `kafka:`；发版 tag 为
  `contrib/watermill-kafka/v{version}`。Kafka record key = `Event.Key` /
  `x-message-key`（分区键）。

### 新增 / 行为

- **EventBus wire 契约**：`RawEvent` ↔ 底层消息单一映射（`x-message-key` /
  `x-event-time` / `x-logical-topic`）；Publish/Subscribe Marshaler 优先级对称。
- **Topic 方法 API**：`Topic.Publish` / `Subscribe` / `PublishRaw`；Bus 解析
  `eventbus.WithBus` → Context → `Default()`；`newLynx` 注入 `SetDefault` 与
  Context，HTTP/gRPC 入站注入 Bus。
- **Watermill Bus**：Start 后动态 `AddConsumerHandler` + `RunHandlers`；去掉
  SignalsHandler；Bus 在 `newLynx` 中先于 Component Start；`lynx.*` 强制
  MemoryTransport，Route 到非内存则失败；`NewFromConfig` 读 `bus:` 段。
- **关停**：Bus last-actor 不变；`AppStopped` 在 `Bus.Stop` 前可投递。
- **Transport Delivery**：`Subscribe` 返回 `<-chan Delivery`（`Event` +
  `Ack`/`Nack`）；Bus 将 Router 对副本消息的确认转达到底层 broker；业务 API
  仍不出现 `Delivery` / `*message.Message`。

---

## v1.4.0 (2026-08-20)

服务注册与发现：核心补齐排水钩子与宣告地址支持，新增 `contrib/registry`
与 `contrib/consul` 两个可选模块。设计见 `docs/design-service-registry.md`，
教程见 `docs/07-registry.md`。

### 新增

- **核心—排水钩子（OnDrain）**：导出 `lynx.ErrDraining`（排水期间
  检查器返回，`errors.Is` 可匹配，供 contrib 模块在排水边沿做注销等
  动作）；`App.OnDrain(fns ...HookFunc)` 注册排水钩子，在排水置位之后
  与排水睡眠**并发**执行；`WithDrainHookTimeout(d)` 设置钩子总预算
  （默认 3s，无钩子不计入上界）。注册钩子后关停时长上界 =
  `max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout + Σ StopTimeout`。
  `boot.Bootstrap` 新增可选 setter `WithDrainHooks`（`New` 签名不变）。
- **核心—ShutdownErrors.Unwrap**：`ShutdownErrors` 实现
  `Unwrap() []error`，`errors.Is`/`errors.As` 可穿透聚合的关停错误。
- **server/http、server/grpc—宣告地址**：`Addr()` 返回实际监听地址
  （随机端口场景为 Listen 成功后的真实地址）；`WithAdvertiseAddr(hostPort)`
  显式设置对外宣告地址、`AdvertiseAddr()` 读取，供服务注册使用，不影响
  实际监听。
- **contrib/registry（新模块）**：服务注册发现数据模型与接口
  （`Instance`/`Endpoint`/`Filter`，`Registry`/`Discovery`/`Watcher`/
  `Advertiser`）；`Registrar` 生命周期服务（Start 注册 + 心跳、Stop/排水
  幂等注销、readiness 集成可开关）；`Resolver`（进程内缓存 + Watch +
  stale 上限）与内置 Picker（round-robin/random）；memory 进程内后端、
  DNS 只读后端（SRV 优先，A/AAAA + 端口表）；`registry://` HTTP
  Transport（`NewHTTPTransport`）与 gRPC resolver（`NewGRPCBuilder`）；
  `NewBackendFromConfig`/`NewFromConfig` 配置驱动，`Bind` 一键注册服务
  并挂排水注销钩子。
- **contrib/consul（新模块）**：Consul 生产后端；`consul.NewFromConfig`
  构造同时实现 `Registry` + `Discovery` 的 `Client`（registry 关闭时
  返回 nil）；支持 ttl/http/grpc 三类 check、blocking Watch（默认
  consistent）、多 Endpoint 经 Meta `lynx_endpoints` 还原。

### 破坏性变更

- **核心—SetFlags 更名 BindFlags**：`SetFlagsFunc` → `BindFlagsFunc`，
  `WithSetFlagsFunc` → `WithBindFlagsFunc`，`DefaultSetFlagsFunc` →
  `DefaultBindFlagsFunc`；`Options.SetFlagsFunc` 字段同步更名。与
  `BindConfigFunc` 命名对齐。调用方迁移：替换标识符即可，签名不变。

### 其他

- 文档与示例：新增 `docs/07-registry.md` 教程、`_examples/registry`
  可运行示例（memory 后端全闭环）；ROADMAP E3 条目更新为「contrib 已
  提供，K8s 仍推荐 DNS/Service」。

## v1.3.0 (2026-08-12)

Context 元数据取值收敛：三个取值函数合并为单入口结构体返回。

### 破坏性变更

- **核心—Context 元数据收敛**：`lynx.NameFromContext(ctx)` /
  `lynx.IDFromContext(ctx)` / `lynx.VersionFromContext(ctx)` 移除，收敛为
  `lynx.Meta(ctx)` 单入口，返回 `lynx.Metadata{Name, ID, Version}` 结构体
  （字段未设置或类型不符时为零值字符串）；内部 context 值由三个独立 key
  合并为单个 `keyMeta`（一次 `WithValue` 写入）。调用方迁移：
  `lynx.NameFromContext(ctx)` → `lynx.Meta(ctx).Name`，余者类推。

### 其他

- 调用点同步：contrib/zap 与 contrib/telemetry 的服务标识字段、_examples/http
  改为 `lynx.Meta(...)`；README、docs 02/03、CLAUDE.md 文档与测试更新。

## v1.2.0 (2026-08-07)

应用入口类型与其职责一致化的命名修正版本：`Builder` 更名 `Runner`，
初始化回调签名简化，`Build()` 收敛为内部实现。

### 破坏性变更

- **核心—Builder 更名 Runner**：命令行入口类型 `lynx.Builder` →
  `lynx.Runner`，`lynx.NewBuilder` → `lynx.NewRunner`；`BuildFunc` →
  `SetupFunc` 且签名由 `func(ctx, app) error` 简化为
  `func(app App) error`（原 ctx 参数与 `app.Context()` 等价，回调内按需
  取用）；`Runner.Build()` 不再对外暴露，收敛为私有 `setupApp()`
  （幂等语义不变）；`ErrBuildFuncNil` → `ErrSetupFuncNil`（消息同步为
  "setup func is nil"）；源文件名 `builder.go` → `runner.go`。

### 其他

- 文档与示例同步：README、docs 01–05 全部代码块改用
  `NewRunner`/`func(app App) error`；示例与测试变量 `builder`/`cli` →
  `runner`；CLAUDE.md 入口节与错误哨兵清单更新。

## v1.1.0 (2026-08-07)

v1.0 发布后的第一个特性版本：补齐 gRPC TLS 一等选项、HTTP 错误约定与防御性
中间件、pprof 运维诊断服务、关停排水（Drain）语义，以及 HTTP/gRPC 客户端组件。
API 冻结（v1.0 导出符号只增不改），全部功能遵循既有生命周期契约（注册先于 Run、
Stop-before-Start 容忍、Start 尊重传入 ctx、Stop 有界超时）。

### 新增

- **gRPC—TLS 一等选项**：`server/grpc.WithTLSConfig(cfg *tls.Config)` 启用
  TLS 传输，与 HTTP 侧同名同义；与 `WithServerOptions(grpc.Creds(...))` 同传
  时 TLSConfig 优先（grpc 对重复 Creds 取最后应用者，实测确认）。
- **HTTP—统一错误约定**：`server/http` 新增
  `ErrorHandler`/`StatusError`/`DefaultErrorHandler`/`HandleFunc`/
  `NewErrorHandler`——业务错误实现 `StatusError` 声明状态码（支持 `errors.As`
  包装查找，被包装错误同样生效），响应体统一
  `{"error":{"message":...}}`（application/json），仅 5xx 记 Error 日志
  （method/path/status/error 四字段）；fn 已写响应头后再报错不二次改写
  （trackedWriter 守卫，无 superfluous WriteHeader）；服务器级默认
  ErrorHandler 定位 v1.2。
- **HTTP—Recovery 中间件**：`http.Recovery()` 捕获链内任意一环（含其余中间件
  与业务 handler）抛出的 panic，记 Error 日志（字段 panic + 完整 stack），
  经 ErrorHandler 写响应（缺省 500 + JSON 错误体）；恢复后连接保持可用，
  后续请求不受影响；建议声明在 `WithMiddleware` 第一个参数（最外层）。
- **HTTP—RateLimit 中间件**：`http.RateLimit(rps)` 服务器级令牌桶限流
  （`golang.org/x/time/rate`），超限写 429 + 统一 JSON 错误体；
  `WithBurst`/`WithRateLimitHandler` 可调；rps ≤ 0 构造期直接 panic
  （配置错误启动阶段暴露）；按路由/IP/用户维度限流定位 v1.2。
- **debug—pprof 运维诊断服务**：新包 `debug`，注册即挂载 `/debug/pprof/*`
  全部标准端点（index/cmdline/profile/symbol/trace + 命名 profiles，自建
  mux 无 `net/http/pprof` 的 DefaultServeMux 副作用）与 `/healthz` 探活；
  缺省仅监听本机回环 `127.0.0.1:6060`（安全警示见包注释与 docs 5.3 节）；
  `Addr()` 返回实际监听地址（随机端口测试可用）；实现 `lynx.Checker`，
  启动成功后才健康；Stop-before-Start 容忍。
- **核心—关停排水（Drain）**：`WithDrainTimeout(d)` 设置排水窗口：关停信号
  到达先置位框架内部 `drainChecker` 使 readiness 聚合（`app.HealthCheckers()`）
  立即失败（LB 摘流），窗口结束才执行真实关停，在途请求收尾不再被截断；
  liveness 端点不消费检查器聚合、排水期间仍 200；与 ShutdownTimeout 是两段
  独立预算，总关停时长上界 = DrainTimeout + ShutdownTimeout + 各服务
  StopTimeout 叠加的既有上界；默认 0 = 不启用，关停行为与 v1.0 完全一致。
- **client/http—HTTP 客户端**：新包 `client/http`——otelhttp 插装（
  `http.DefaultTransport` 浅克隆，不修改进程全局）、`request_id`/`user_id`
  经 `X-Request-Id`/`X-User-Id` 请求头传播（已存在的同名头不覆盖，与
  server/http `WithRequestID` 还原形成全链路闭环）、整体超时 30s、
  `WithRetry` 指数退避重试（传输层错误或 429/502/503/504，429/503 遵守
  Retry-After；`req.GetBody` 可重放才重试带 body 请求）；Do 不读不关响应体。
- **client/grpc—gRPC 客户端**：新包 `client/grpc`，`Dial` 包装
  `grpc.NewClient`（惰性连接，不发起握手）——otelgrpc client stats handler
  插装、unary/stream 拦截器把日志属性（request_id/user_id）写入 outgoing
  metadata（已有 key 不覆盖）、per-RPC 默认调用超时 30s、`WithTLSConfig`
  与 server 侧 F1 同名同义；服务端 metadata 还原入 v1.2 backlog（边界已在
  GoDoc 写明）。

### 其他

- 文档：新增 `docs/06-clients.md`（HTTP/gRPC 客户端两节 + 传播闭环时序说明）；
  docs/05 新增错误处理约定、防御性中间件（5.4.8）、debug 运维服务（5.3）节，
  gRPC 配置节补 `WithTLSConfig` 并删除"无 TLS 一等选项，仅逃生口"旧表述；
  docs/03 生命周期节补 drain 时序（文字版）；README 功能清单与目录树更新。
- 新依赖：`golang.org/x/time`（RateLimit 令牌桶，准标准库）。
- 仓库卫生：移除 v1.1 实施计划交接文档（发布卫生约束）。

## v1.0.0 (2026-08-05)

Lynx 首个稳定版本。核心生命周期、服务系统、配置系统 API 冻结，此后保持向后兼容。

v1.0 发布前完成了大规模 API 重构（breaking changes 无需向后兼容），本版本即包含
重构后的最终设计。

### 破坏性变更（v1.0 前的最后机会）

- **核心—服务接缝收窄**：`Init(app App) error` → `Init(ctx AppContext) error`。新增
  `lynx.AppContext` 接口（`Context`/`Config`/`Logger`/`HealthCheckers`/`Close`），
  `App` 是 `AppContext` 的超集；服务与测试只需实现 AppContext，不再面对完整 App。
  生命周期接口 `LifecycleManaged` 重命名为 `Lifecycle`。
- **核心—关停错误对称上抛**：`Stop(ctx)` → `Stop(ctx) error`。服务 Stop
  返回的错误与超时错误（受 `Options.StopTimeout` 约束）聚合进
  `ShutdownErrors`，与 OnStop 钩子错误一起由 `Run()` 统一上抛。
- **核心—砍掉 gocloud.dev**：`gocloud.dev/server/health.Checker` →
  `lynx.Checker`（本地定义）；`App.HealthCheckFunc() HealthCheckFunc` →
  `App.HealthCheckers() []Checker`；server 服务的健康检查配置改为
  `lynx.HealthCheckersFunc`（`http.WithHealthCheckers(app.HealthCheckers)`）。
  `server/http` 的 liveness/readiness handler 与 requestlog（Entry/NewHandler）
  全部本地实现；全模块移除 gocloud.dev 依赖。
- **核心—日志统一**：移除 `github.com/lynx-go/x/log`，框架与服务日志统一走
  slog（服务在 `Init(ctx)` 用 `ctx.Logger(...)` 取实例）。`SetLogger` 保留
  `slog.SetDefault` 同步并已在接口注释声明该全局副作用；`--log-level` 对
  框架与应用日志一致生效。新增导出 `lynx.LogLevelFromConfig` /
  `lynx.ParseLogLevel`（键优先级：`logging.level` → `log-level` → `log_level`，
  与 zap 共用，消除了两处优先级相反的实现）。
- **核心—默认启用配置 flags**：`Options.EnsureDefaults` 默认设置
  `DefaultSetFlagsFunc`/`DefaultBindConfigFunc`（`-c/--config` 等参数开箱即
  用，不再静默失效）；未知 flag 忽略（`go test` 二进制的 `-test.*`）；
  `--help` 以初始化错误返回；新增 `WithDisableConfigFlags()` opt-out；
  **删除** `WithUseDefaultConfigFlagsFunc()`。
- **核心—Builder/Options/errors 修正**：`Builder.Build() App` →
  `Builder.Build() (App, error)`（消除 `.Register(...)` nil 解引用陷阱）；
  `ErrCloseTimeoutTooSmall/Large` → `ErrShutdownTimeoutTooSmall/Large`；
  `NewOptions` 改为 `&Options{}` + `EnsureDefaults()` + 应用选项（补齐
  Name/StopTimeout 双轨默认值）；删除 `lynx.Option()` 死方法；`errorf`
  包装类型改用 `errors.New`。
- **核心—配置键命名空间**：应用元信息仅从
  `service.name`/`service.id`/`service.version` 读取（配置值覆盖
  Options 对应值）；旧顶层 `name`/`id`/`version` 键回退已移除。
- **核心—Register 防御**：注册 plain nil 服务返回明确错误
  （`cannot register nil service`）而非运行时 panic。
- **HTTP**：删除 `http.NewRouter()`（`http.NewServeMux()` 纯别名，直接使用
  标准库）；`WithHealthCheck` → `WithHealthCheckers`。
- **contrib/metrics → contrib/telemetry**：模块目录、包名、module path 全部
  重命名（`github.com/lynx-go/lynx/contrib/telemetry`）；默认 trace exporter
  改为 noop（生产忘配 exporter 不再向 stdout 倒 trace），新增
  `telemetry.WithStdoutTrace()` 供开发调试；`Init(ctx)` 在未显式
  `WithResource` 时自动以应用名构建 `service.name` 资源属性；`Stop` 返回
  关停错误。
- **contrib/zap**：`NewZapLoggerToFile` 合并进
  `NewZapLogger(logLevel string, outputs ...string)`（默认 stdout）；
  `getLevel` 改用 `lynx.LogLevelFromConfig`；构造函数参数 `lynx.App` →
  `lynx.AppContext`。
- **contrib/schedule**：`Start` 改为尊重传入 ctx（`<-ctx.Done()` 返回 nil，
  对齐 run.Group actor 语义）；删除内部 ctx/`ensureCtx`/`WithoutCancel`
  机制；`Stop` 返回 error；Stop-before-Start 容忍语义保留。
- **contrib/kafka / pubsub**：`Init`/`Stop` 签名适配（AppContext、Stop error）；
  服务日志改 `ctx.Logger`（kafka 删除 `t.app` 字段，pubsub 删除 `broker.app`
  死字段）；`kafka.NewFromConfig` 的 `(nil, nil)` 契约保留，返回 nil 时
  不得 Register（配合框架 nil 检查得到明确错误）。
- `pubsub.Transport.Publish` 增加 `ctx` 参数（trace/元数据传播）
- `pubsub.NewBroker` 返回 `Broker` 接口而非未导出类型
- 删除遗留 Deprecated API：`SetMessageKey` / `GetMessageKey` / `SetMessageID` / `GetMessageID`
- `command` 重试耗尽错误文案调整为 `timed out waiting for dependencies to be healthy`
- **核心—超时常量更名**：`MinShutdownTimeout`/`MaxShutdownTimeout` →
  `MinTimeout`/`MaxTimeout`（两者是 ShutdownTimeout 与 StopTimeout 共用的
  校验区间，原名误导）
- **核心—NewTraceHandler 迁移**：`lynx.NewTraceHandler` 移入新增 `logging`
  子包（`logging.NewTraceHandler`），根包不再提供日志装饰器
- **Kafka—XDGSCRAMClient 内部化**：SCRAM 客户端实现类型改为未导出
  （`xdgSCRAMClient`），经 `sasl.mechanism` 配置启用，用户无需直接引用
- **PubSub—配置 schema**：`pubsub` 段由 `routes` 改为 `events`（逻辑
  topic → 事件配置：`route: {transport, key}` + 事件级选项
  `log_message`/`auto_ack`/`continue_on_error`/`group`/`instances`/
  `retry`）；重试中间件由全局改为 per-handler 挂载，事件级重试配置
  生效；`log_message` 改为 publish/subscribe 两侧独立的映射形态

### 修复

- **HTTP—Stop 超时竞态**：`Shutdown` 因 deadline 返回与 `ctx.Done()` 随机
  选边时，超时曾被当作正常关停放行（返回 nil，未完成的连接被悄悄遗弃）。
  现在两种分支统一走超时路径：强制关闭活动连接并返回
  `http server graceful shutdown timed out` 包装错误。新增回归测试：
  永不结束的 handler + 极短 ShutdownTimeout，`-race -count=20` 下断言
  Stop 恒返回超时错误、连接被强制关闭。
- **gRPC—reflection 注册时机**：`reflection.Register` 从 `Start` 移到
  `NewServer`（`Serve` 之后注册服务会 panic，二次 Start 必崩的 latent bug
  根除）。
- **服务契约—Start 尊重传入 ctx**：`contrib/kafka` 的 `Start` 双监听内部
  ctx 与传入 ctx；`contrib/pubsub` 的 `Router` 删除内部 ctx，`Start` 阻塞在
  传入 ctx、`Stop` 直接返回 nil——对齐全库契约，框架调整中断顺序（先
  cancel 后 Stop）不再有挂死风险。
- **gRPC—选项命名对齐**：`WithHealthCheck` → `WithHealthCheckers`（与 HTTP
  侧一致，均接受 `lynx.HealthCheckersFunc`）。
- **pubsub Router—nil 防御补全**：`Router.Init` 整体容忍 nil `AppContext`
  （logger 与订阅 ctx 取兜底值），脱离框架单用不再有 latent panic；补
  `Init(nil)` 回归用例。
- **Kafka**：修复真实发布 100% 失败的双重缺陷（缺省 Marshaler 与
  `Producer.Return.Successes` 未设置）；新增 SASL（PLAIN/SCRAM-SHA-256/512）
  与 TLS（CA/SNI/skip-verify）认证配置；consumer/producer 参数按侧独立缓存
- **PubSub**：`Broker.Start` 两阶段提交，部分注册失败后补充 Route 重试不再
  panic；订阅 handler 重名在缓冲期即报错；重试次数/退避可配置
- **核心**：服务 Stop 有界超时（`Options.StopTimeout`）；`Init` 在锁外执行
  （Init 内调用 App 方法不再死锁）；Init/OnStart 失败逆序清理已初始化服务；
  OnStop 错误随 `Run()` 上抛；退出信号提前注册
- **Schedule**：Stop/Start 竞态导致的关闭永久挂起修复；新增时区与任务错误回调
- **HTTP**：脱钩 gocloud.dev/server 的 HTTP server 实现（显式注入 otel provider，
  消除进程全局副作用）；liveness/readiness handler 与 requestlog 全部本地
  实现；新增 TLS、IdleTimeout 与 `*http.Server` 逃生口
- **gRPC**：app 级健康检查轮询同步到 `grpc.health.v1`；Recovery 移至拦截器链
  最外层；新增流式拦截器入口
- **Metrics**：重复注册报错；支持注入 OTel Resource
- **Zap**：`NewLogger`/`NewSyncableLogger` 去重；级别键与框架统一
- **核心—--log-level 默认值**：默认 flag 由 "info" 改为空——未显式传入时
  不再遮蔽配置文件的 `logging.level`/`log_level` 键；`DefaultBindConfigFunc`
  同时把工作目录加入配置搜索路径（viper v1.17+ 不再隐式搜索 "."）
- **Kafka—Subscriber Unmarshaler**：显式装配 `DefaultMarshaler`
  （watermill-kafka v3.1.x 缺省报 "missing unmarshaler"）
- **Zap—Linux 标准流 Sync**：`Sync`/`SyncOnStop` 忽略 stdout/stderr 的
  fsync EINVAL（Linux 上 `fsync(/dev/stdout)` 恒失败，zap 已知问题
  uber-go/zap#328）；修复前 Linux 应用每次关停 OnStop 钩子都会报错

### 新增

- **PubSub 透明序列化**：`Publish` 直接接受业务对象自动序列化（默认 JSON，
  可注入自定义 `Marshaler`）；`pubsub.Subscribe[T]` 类型化订阅自动反序列化；
  字节级 `*Message` 语义保留
- **PubSub 类型化 Handler**：`NewTypedHandler[T]`/`NewHandler` 工厂构造
  `pubsub.Handler`（`EventName`/`HandlerName`/`NewEvent`/`Handle`），
  `NewEvent()` 声明式解码 + `MessageDecoder` 免反射泛型擦除，Pub/Sub 两侧
  Marshaler 解析对称
- **全链路日志**：新增 `logging` 子包（`NewTraceHandler`/`NewAttrsHandler`
  装饰器、`WithAttrs`/`AttrsFrom` 请求级属性传播、`FieldRequestID`/
  `FieldUserID` 标准键）；`http.WithRequestID()` 中间件生成/透传
  `X-Request-Id` 并写入请求 ctx；请求日志 `Entry.RequestID` 与业务日志
  关联；pubsub 跨请求传播（`Options.PropagateAttrs` 白名单，缺省
  request_id/user_id，Publish 写入消息头、Subscribe 还原进 ctx）
- **PubSub 配置驱动装配**：新增导出类型 `EventOptions`/`LogMessageOptions`；
  新增配置键 `pubsub.debug`（watermill 核心 debug 日志开关，缺省关闭）、
  `pubsub.events`、`pubsub.log_message`（全局收发日志默认值）

### 其他

- Go 最低版本升至 1.26.5（7 模块 go.mod 与 go.work、CI）：修复
  govulncheck 在 Go 1.25.0 报出的 23 个标准库已调用漏洞
- CI 矩阵修复：`contrib/metrics` → `contrib/telemetry`（模块更名未同步，
  telemetry 此前无 CI 覆盖）
- 行为说明：contrib/zap 日志字段对齐 `service.name`/`service.id`/
  `service.version`
- 各 contrib 模块独立 LICENSE
- 发布流程：`task release-all --Version=v1.0.0`（见 RELEASE.md）
- 仓库卫生：`_examples` 清理编译产物（cli.exe/cli.out/pubsub.exe/schedule.exe
  不再出现在工作区）；`wire_gen.go` 移除残留的 `go-sql-driver/mysql` 空导入；
  `cli/config.yaml` 删除未使用的 `addr`；全部 7 模块（根、_examples、5 个
  contrib）`go mod tidy`，无 `gocloud.dev` / `lynx-go/x` 残留（根模块
  otelhttp 转为直接依赖、`google/wire` 移除、schedule/zap 及 `_examples` 的
  go.sum 补齐缺失的 `.go.mod` 校验行）；`_examples` 对 lynx 及 5 个 contrib
  的 require 统一为 `v1.0.0`；CI 覆盖率门槛由仅核心模块扩展为根与全部
  contrib 统一 70%（`_examples` 除外）；`Taskfile.yml` 默认发布版本更新为
  `v1.0.0`
- 行为说明：gRPC 服务器 Start/Stop 路径与请求拦截器路径的日志均经
  `s.logger` 输出（`WithLogger` 配置的实例，缺省 `slog.Default()`），
  两侧实例一致（见 `WithLogger` GoDoc）

## v0.7.2 及之前

见 git 历史（未维护独立 changelog）。
