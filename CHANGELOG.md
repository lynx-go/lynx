# Changelog

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
