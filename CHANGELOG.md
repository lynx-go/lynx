# Changelog

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
