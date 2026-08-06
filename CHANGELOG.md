# Changelog

## Unreleased

v1.0 发布前 API 重构（尚未发版，breaking changes 无需向后兼容）。

### 破坏性变更

- **核心—组件接缝收窄**：`Init(app App) error` → `Init(env Env) error`。新增
  `lynx.Env` 接口（`Context`/`Config`/`Logger`/`HealthCheckers`/`Close`），
  `App` 是 `Env` 的超集；组件与测试只需实现 Env，不再面对完整 App。
  生命周期接口 `LifecycleManaged` 重命名为 `Lifecycle`。
- **核心—关停错误对称上抛**：`Stop(ctx)` → `Stop(ctx) error`。组件 Stop
  返回的错误与超时错误（受 `Options.StopTimeout` 约束）聚合进
  `ShutdownErrors`，与 OnStop 钩子错误一起由 `Run()` 统一上抛。
- **核心—砍掉 gocloud.dev**：`gocloud.dev/server/health.Checker` →
  `lynx.Checker`（本地定义）；`App.HealthCheckFunc() HealthCheckFunc` →
  `App.HealthCheckers() []Checker`；server 组件的健康检查配置改为
  `lynx.HealthCheckersFunc`（`http.WithHealthCheckers(app.HealthCheckers)`）。
  `server/http` 的 liveness/readiness handler 与 requestlog（Entry/NewHandler）
  全部本地实现；全模块移除 gocloud.dev 依赖。
- **核心—日志统一**：移除 `github.com/lynx-go/x/log`，框架与组件日志统一走
  slog（组件在 `Init(env)` 用 `env.Logger(...)` 取实例）。`SetLogger` 保留
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
- **核心—配置键命名空间**：name/id/version 读取顺序改为
  `service.name`/`service.id`/`service.version` 优先，顶层旧键回退
  （过渡期，deprecated）。
- **核心—Register 防御**：注册 plain nil 组件返回明确错误
  （`cannot register nil component`）而非运行时 panic。
- **HTTP**：删除 `http.NewRouter()`（`http.NewServeMux()` 纯别名，直接使用
  标准库）；`WithHealthCheck` → `WithHealthCheckers`。
- **contrib/metrics → contrib/telemetry**：模块目录、包名、module path 全部
  重命名（`github.com/lynx-go/lynx/contrib/telemetry`）；默认 trace exporter
  改为 noop（生产忘配 exporter 不再向 stdout 倒 trace），新增
  `telemetry.WithStdoutTrace()` 供开发调试；`Init(env)` 在未显式
  `WithResource` 时自动以应用名构建 `service.name` 资源属性；`Stop` 返回
  关停错误。
- **contrib/zap**：`NewZapLoggerToFile` 合并进
  `NewZapLogger(logLevel string, outputs ...string)`（默认 stdout）；
  `getLevel` 改用 `lynx.LogLevelFromConfig`；构造函数参数 `lynx.App` →
  `lynx.Env`。
- **contrib/schedule**：`Start` 改为尊重传入 ctx（`<-ctx.Done()` 返回 nil，
  对齐 run.Group actor 语义）；删除内部 ctx/`ensureCtx`/`WithoutCancel`
  机制；`Stop` 返回 error；Stop-before-Start 容忍语义保留。
- **contrib/kafka / pubsub**：`Init`/`Stop` 签名适配（Env、Stop error）；
  组件日志改 `env.Logger`（kafka 删除 `t.app` 字段，pubsub 删除 `broker.app`
  死字段）；`kafka.NewFromConfig` 的 `(nil, nil)` 契约保留，返回 nil 时
  不得 Register（配合框架 nil 检查得到明确错误）。

### 仓库卫生

- `_examples` 清理编译产物（cli.exe/cli.out/pubsub.exe/schedule.exe 不再
  出现在工作区）；`wire_gen.go` 移除残留的 `go-sql-driver/mysql` 空导入；
  `cli/config.yaml` 删除未使用的 `addr`。
- 全部 7 模块（根、_examples、5 个 contrib）`go mod tidy`，无
  `gocloud.dev` / `lynx-go/x` 残留。

## v1.0.0 (2026-08-05)

Lynx 首个稳定版本。核心生命周期、组件系统、配置系统 API 冻结，此后保持向后兼容。

### 破坏性变更（v1.0 前的最后机会）

- `pubsub.Transport.Publish` 增加 `ctx` 参数（trace/元数据传播）
- `pubsub.NewBroker` 返回 `Broker` 接口而非未导出类型
- 删除遗留 Deprecated API：`SetMessageKey` / `GetMessageKey` / `SetMessageID` / `GetMessageID`
- `command` 重试耗尽错误文案调整为 `timed out waiting for dependencies to be healthy`

### 修复

- **Kafka**：修复真实发布 100% 失败的双重缺陷（缺省 Marshaler 与
  `Producer.Return.Successes` 未设置）；新增 SASL（PLAIN/SCRAM-SHA-256/512）
  与 TLS（CA/SNI/skip-verify）认证配置；consumer/producer 参数按侧独立缓存
- **PubSub**：`Broker.Start` 两阶段提交，部分注册失败后补充 Route 重试不再
  panic；订阅 handler 重名在缓冲期即报错；重试次数/退避可配置
- **核心**：组件 Stop 有界超时（`Options.StopTimeout`）；`Init` 在锁外执行
  （Init 内调用 App 方法不再死锁）；Init/OnStart 失败逆序清理已初始化组件；
  OnStop 错误随 `Run()` 上抛；退出信号提前注册
- **Schedule**：Stop/Start 竞态导致的关闭永久挂起修复；新增时区与任务错误回调
- **HTTP**：脱钩 gocloud.dev/server 的 HTTP server 实现（显式注入 otel provider，
  消除进程全局副作用）；**保留** gocloud.dev/server 的 `health.Checker` 抽象与
  `requestlog` 包（v1.0 决策：依赖保留，仅实现脱钩）；
  新增 TLS、IdleTimeout 与 `*http.Server` 逃生口
- **gRPC**：app 级健康检查轮询同步到 `grpc.health.v1`；Recovery 移至拦截器链
  最外层；新增流式拦截器入口
- **Metrics**：重复注册报错；支持注入 OTel Resource
- **Zap**：`NewLogger`/`NewSyncableLogger` 去重；级别键与框架统一

### 新增

- **PubSub 透明序列化**：`Publish` 直接接受业务对象自动序列化（默认 JSON，
  可注入自定义 `Marshaler`）；`pubsub.Subscribe[T]` 类型化订阅自动反序列化；
  字节级 `*Message` 语义保留

### 其他

- 各 contrib 模块独立 LICENSE
- 发布流程：`task release-all --Version=v1.0.0`（见 RELEASE.md）
- 发布卫生：全部 7 模块 `go mod tidy`（根模块 otelhttp 转为直接依赖、
  `google/wire` 移除、schedule/zap 及 `_examples` 的 go.sum 补齐缺失的
  `.go.mod` 校验行）；`_examples` 对 lynx 及 5 个 contrib 的 require 统一为
  `v1.0.0`；CI 覆盖率门槛由仅核心模块扩展为根与全部 contrib 统一 70%
  （`_examples` 除外）；`Taskfile.yml` 默认发布版本更新为 `v1.0.0`
- 行为说明：gRPC 服务器 Start/Stop 路径日志经 `log.InfoContext` 输出，ctx
  无注入 logger 时回退 `slog.Default()`，与 `WithLogger` 配置的请求日志
  实例可能不同（见 `WithLogger` GoDoc）

## v0.7.2 及之前

见 git 历史（未维护独立 changelog）。
