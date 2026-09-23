# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Lynx is a lightweight Go microservice framework built on Go 1.26+ that provides application lifecycle management, a service-based architecture, and integrations for HTTP servers, messaging (EventBus / Watermill / Kafka), scheduling, and configuration management.

## Development Commands

### Building and Running

```bash
# Run examples
cd _examples/http && go run main.go --addr=:8080
cd _examples/cli && go run . set -c config.yaml a 1   # 多命令 + Wire 双聚合示例（version/set/get/list）
cd _examples/bus && go run main.go
cd _examples/bus-kafka && go run .   # watermill+kafka 跨进程 Bus（WithBusProvider，需本地 kafka，见其 README）
cd _examples/schedule && go run main.go
cd _examples/boot && go run main.go

# Generate Wire dependency injection code
cd _examples/boot && wire
# Or use go generate
go generate ./...
```

### Release Management

Uses mise for releases:

```bash
# Release all modules at once (tags main repo and all contrib modules)
mise run release-all -- v1.2.0 "release v1.2.0"

# Individual module releases
mise run release-tag -- v0.5.8 "release message"
# contrib 模块的前缀由 version 参数携带：
mise run release-tag -- contrib/watermill-kafka/v0.5.8 "release message"
```

`VERSION` / `COMMENT` 环境变量可替代位置参数（如 `VERSION=v1.2.0 COMMENT="release v1.2.0" mise run release-all`）。

The project uses a multi-module release strategy. When releasing, you must tag:
- Main repo: `v{version}`
- contrib/zap: `contrib/zap/{version}`
- contrib/watermill: `contrib/watermill/{version}`
- contrib/watermill-kafka: `contrib/watermill-kafka/{version}`
- contrib/telemetry: `contrib/telemetry/{version}`
- contrib/schedule: `contrib/schedule/{version}`
- contrib/registry: `contrib/registry/{version}`
- contrib/consul: `contrib/consul/{version}`
- contrib/cluster: `contrib/cluster/{version}`
- contrib/cluster-redis: `contrib/cluster-redis/{version}`

### Module Structure

This is a Go workspace using `go.work`. The main modules are:
- `./` - Core lynx framework（含 `eventbus/`）
- `./_examples` - Example applications
- `./contrib/zap` - Zap logger integration
- `./contrib/watermill` - Watermill-driven `eventbus.Bus`（`NewFromConfig` 读 `bus:` 段）
- `./contrib/watermill-kafka` - Kafka Transport service (watermill-kafka/v3)，package `kafka`，实现 `eventbus.Transport`
- `./contrib/telemetry` - OpenTelemetry lifecycle management (trace/metrics providers)
- `./contrib/schedule` - Cron scheduler；`Exclusive` 任务经 `cluster.TryOnce` 按格子互斥
- `./contrib/cluster` - 进程间协调：`Coordinator`（Claim/Acquire）、`TryOnce`、`Campaign`、`Singleton`
- `./contrib/cluster-redis` - Redis 实现 `cluster.Coordinator`（仅协调，不是业务 Redis 客户端）
- `./contrib/registry` - Service registry/discovery: types, Registrar, Resolver (with consumer-side `Subscribe`), Pickers, memory/DNS backends, `registry://` HTTP transport & subscription-driven gRPC resolver
- `./contrib/consul` - Consul registry/discovery backend（`consul.NewFromConfig`），并提供 `Client.Coordinator()` 实现 `cluster.Coordinator`

Server implementations (within main module):
- `./server/http` - HTTP server using stdlib `net/http` with otelhttp instrumentation
- `./server/grpc` - gRPC server with interceptors
- Shared server rules live in `internal/serverkit`: health-check execution, bounded graceful shutdown (caller deadline ∩ configured cap), request-id/user_id propagation (shared wire keys `x-request-id`/`x-user-id`), and lifecycle events (`lynx.server.listening/stopping/stopped`, `ServerEvent.Service` distinguishes http/grpc/debug)

Client implementations (within main module):
- `./client/http` - HTTP client: otel instrumentation, request_id/user_id propagation, timeout + retry (backoff/v5), optional circuit breaker (`WithCircuitBreaker`, gobreaker/v2 wrapped behind lynx-owned options)
- `./client/grpc` - gRPC client: otel, request_id/user_id metadata propagation, per-RPC timeout

Each contrib module has its own `go.mod` with local replace directives pointing to `../../` for the main lynx module.

## Architecture

### Core Abstractions

**Service System**
All managed units implement the `Service` interface (service.go):
```go
type Service interface {
    Name() string
    Lifecycle
}

type AppContext interface {
    Context() context.Context
    Config() Config
    Logger(kwargs ...any) *slog.Logger
    HealthCheckers() []Checker
    Bus() eventbus.Bus
    Close()
}

type Lifecycle interface {
    Init(ctx AppContext) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

Services are registered via `app.Register(...)` and automatically managed through their lifecycle. Services implementing `lynx.Checker` (`CheckHealth() error`, defined locally in health.go — no gocloud.dev dependency) are automatically added to health checks; `app.HealthCheckers()` returns the snapshot slice. `Stop` errors are collected (bounded by `Options.StopTimeout`) and surfaced by `Run()` together with OnPreStop hook errors.

Optional `lynx.Ready` (`Ready() <-chan struct{}`): close the channel after the service has entered the running state (HTTP/gRPC/debug: after `Listen`, before `Serve`). Listen/Start failure must not close it. All readiness probes are bounded (ready.go is the single owner): a Ready channel that never closes is a timeout, not a hang.

**OrderedServices**
`lynx.OrderedServices(name, svcs...)` wraps multiple services as one `Service`. Init/Start run in argument order; Stop is reverse. Nested groups are allowed. Children must not also be `Register`'d.

Start sequencing after launching each child `Start` in its own goroutine:
1. `Ready` → wait until the channel closes, bounded by the same budget (timeout 10s)
2. else `Checker` → poll `CheckHealth` until nil, each call bounded by the remaining budget (timeout 10s)
3. else proceed immediately after `Start` is invoked

The wrapper itself implements `Checker` (aggregates children) and `Ready` (closes after all children are ready). Top-level services registered on the App still start concurrently with each other.

**ServiceFactory**
For dynamic service creation with configurable instance counts (service.go:40-55):
```go
type ServiceFactory interface {
    New() Service
    Options() FactoryOptions
}
```

**Hooks & Registration**
Lifecycle hooks and services are registered via direct methods on the `App` interface (lynx.go). Hook phases, in firing order (Pre/Post anchor the service group's start/stop): `OnPreStart` → (services start) → `OnPostStart` → [drain window: `OnDrain`] → `OnPreStop` → (services stop, bus stops) → `OnPostStop`:
- `app.OnPreStart(fns ...HookFunc)` - Runs before services start (rename of OnStart in v1.10.0); first error aborts startup
- `app.OnPostStart(fns ...HookFunc)` - "Running notification": fires after every service actor has entered its execute body (Start invocation follows immediately; blocking servers count at actor entry — this is NOT readiness); hook error triggers app shutdown
- `app.OnDrain(fns ...HookFunc)` - Runs concurrently with the drain sleep (e.g. registry deregistration); the drain window IS the hooks' total budget (DrainTimeout, v1.10.0 merged DrainHookTimeout into it). Requires DrainTimeout > 0: registering hooks with DrainTimeout=0 makes Run() fail fast with ErrDrainHooksRequireDrainTimeout
- `app.OnPreStop(fns ...HookFunc)` - Runs before services Stop, while they still serve in-flight requests (rename of OnStop in v1.10.0); budget ShutdownTimeout
- `app.OnPostStop(fns ...CleanupFunc)` - Final cleanup AFTER all services and the bus have stopped, before Run returns; LIFO order, budget CleanupTimeout (default 10s, `WithCleanupTimeout`); covers ALL Run exit paths (incl. init failure), Close() is the fallback when Run is never called; runs exactly once per app. `CleanupFunc` is `func()` — terminal-phase errors have no consumer. Wire's generated cleanup (closing DB/Redis pools) belongs HERE, not in OnPreStop (pools are still used by in-flight requests during drain/shutdown)
- `app.Register(services ...Service)` - Register services (Init runs synchronously at registration; the first error is recorded and returned by `Run()`). All registration must happen before `Run()` and before `Close()`: after `Run()` starts, `Register`/`RegisterFactories` panic and `Command` returns an error; after `Close()`, the same rejection applies (panic / `ErrAppClosed`)
- `app.RegisterFactories(factories ...ServiceFactory)` - Register service factories
- `app.Command(cmd CommandFunc, opts ...CommandOption)` - Register a one-shot CLI command (options: `WithMaxTries`/`WithBackoff`/`WithProbeTimeout`/`WithCommandName`)

**Application Lifecycle**
The main run loop lives in lifecycle.go: a lifecycle module owns actor scheduling, phase ordering, registration state, and error aggregation (oklog/run has been removed):
1. Executes OnPreStart hooks
2. Runs all services concurrently as actors (each service gets its own goroutine); fires OnPostStart hooks once every service actor has entered its execute body
3. Waits for the first trigger: any actor returning (Start failure, Command completion), a shutdown signal (SIGTERM, SIGQUIT, SIGINT), `Close()`, or an OnPostStart hook error
4. Shutdown phases in fixed order, independent of actor registration order: drain window (OnDrain hooks concurrent with DrainTimeout sleep) → cancel context → OnPreStop hooks (ShutdownTimeout budget) → services stopped in reverse registration order (LIFO, bounded by StopTimeout) → `AppStopped` event → bus stopped (bounded) → all errors aggregated into the `Run()` return value
5. OnPostStop cleanup hooks (CleanupTimeout budget, LIFO) run on every exit path; Init/OnPreStart failure paths stop already-initialized services in reverse order and skip the drain/OnPreStop phases (they only run once services have entered the running phase)

Optional drain window (`Options.DrainTimeout`, default 0 = disabled): on shutdown, an internal `drainChecker` is set so readiness aggregation (`app.HealthCheckers()`) fails immediately (LB 摘流), then the app sleeps `DrainTimeout` before cancelling the context and proceeding with the v1.0 shutdown sequence. During the drain window checkers return the exported `lynx.ErrDraining`. `app.OnDrain(fns...)` hooks (e.g. registry deregistration) run **concurrently** with the drain sleep, bounded by the window itself (v1.10.0 removed the separate DrainHookTimeout). Registering OnDrain hooks with DrainTimeout=0 fails fast at Run() start with `ErrDrainHooksRequireDrainTimeout`. DrainTimeout is a separate budget from ShutdownTimeout: total shutdown upper bound = DrainTimeout + ShutdownTimeout + StopTimeout stack + CleanupTimeout. Drain only affects readiness (HTTP `/healthz/liveness` never consumes checkers).

**Context Values**
The application context carries standard values (lynx.go):
- `Meta(ctx)` returns a `Metadata{Name, ID, Version}` struct from the context
- Fields map to `service.name` / `service.id` (hostname by default) / `service.version`

### Configuration System

Configuration is exposed through two generic interfaces, decoupled from the underlying library (the default implementation adapts `*viper.Viper` via `lynx.NewViperConfig`):

- `lynx.Config` - read-only config access, returned by `app.Config()`: `Get(path)` (dot-separated paths), typed getters (`GetString`/`GetBool`/`GetInt`/`GetStringMap`/`GetStringSlice`), `IsSet`, `Unmarshal(out)`
- `lynx.ConfigSource` - superset of `Config`, received by `BindConfigFunc`: adds `Set`, `SetFile`, `AddSearchPath`, `SetFileFormat`, `SetEnvPrefix`, `AutomaticEnv`, `BindEnv`

Other config libraries (e.g. koanf) can be integrated by implementing these two interfaces.

Decode semantics (`Unmarshal`/`UnmarshalKey`): struct targets default to struct-driven leaf-wise `Get` — tag fallback chain mapstructure → json → lowercase field name; env-only keys (set only in env vars) participate. Non-struct targets fall back to viper semantics. Options: `WithTagName` (restrict tag), `WithStrictTypes` (reject non-string-scalar→collection weak conversion, e.g. `brokers: 42`; string sources/env stay legal), `WithErrorUnused` (report unknown keys in the subtree; `UnmarshalKey` only). Nested container fields (maps/slices of structs, incl. `mapstructure:",remain"`) decode elements by mapstructure semantics. contrib fromconfig constructors（registry/consul/watermill-kafka）统一走 `WithStrictTypes`，不再各自手写类型预检垫片；`registry.FileConfig`/`LoadFileConfig` 是 `registry.*` 段的唯一 schema（consul 经它读共享字段）。

Configuration flow:
1. `BindFlagsFunc` - Bind CLI flags
2. `BindConfigFunc` - Bind flags to the app ConfigSource, set config file paths
3. Flags are parsed, config file is read, env vars are bound

Default flags are enabled by default (`Options.EnsureDefaults` sets `DefaultBindFlagsFunc`/`DefaultBindConfigFunc`); opt out with `WithDisableConfigFlags()`. For externally-parsed args (subcommand CLIs), `lynx.WithConfigFile(path)` binds the config file path AND disables default flags in one option (order-trap-free replacement for the manual `WithDisableConfigFlags` + `WithBindConfigFunc` pair). Unknown flags are ignored (test binaries' `-test.*` args). `--help` returns an init error handled by `Runner.Run` exit code.

Default flags (see `DefaultBindFlagsFunc` in lynx.go):
- `--config/-c` - Config file path
- `--config-type` - File type (yaml, json, etc.)
- `--config-dir` - Config directory
- `--log-level` - Log level

App metadata keys: `service.name`/`service.id`/`service.version` (the legacy top-level `name`/`id`/`version` fallback was removed in v1.0). Log level keys: `logging.level` → `log-level` → `log_level` (`lynx.LogLevelFromConfig`).

### Boot/Bootstrap Pattern

The `boot` package provides a structured way to organize application initialization using Wire dependency injection:

1. Create provider functions for dependencies (see _examples/boot/provides.go)
2. Define a Wire injector function with `//go:build wireinject` tag
3. Register providers in a ProviderSet
4. Wire generates the dependency graph
5. Bootstrap.Apply(app) registers all hooks/services with the app

This pattern is particularly useful for complex applications with many services.

### Key Services

**HTTP Server** (server/http/server.go)
- Wraps stdlib `net/http.Server` with otelhttp instrumentation (health check handlers and request log are implemented locally — no gocloud.dev dependency)
- Support for request logging and custom timeouts
- Automatically registers health check endpoints at `/healthz/liveness` and `/healthz/readiness` (prefix/disabled via `WithHealthCheckPrefix`/`WithDisableHealthCheck`; checkers run concurrently with a per-check timeout, default 3s, `WithHealthCheckTimeout`)
- `Serve` returning `http.ErrServerClosed` on normal shutdown is normalized to nil (no spurious `lynx.service.failed` events); 5xx error bodies are generic (`http.StatusText`), details go to logs only
- Request-id/user_id propagation is installed by default (opt out with `WithDisableRequestID`): incoming `x-request-id`/`x-user-id` are validated and restored into ctx log attrs; the response echoes `x-request-id`

**gRPC Server** (server/grpc/server.go)
- Wraps `google.golang.org/grpc` with health check and reflection
- Built-in interceptors in chain order: recovery (outermost), request_id/user_id propagation restore (from incoming metadata keys `x-request-id`/`x-user-id` into ctx log attrs, `interceptor.RequestIDPropagation`), then request logging (`WithRequestLog`/`WithRequestLogLevel`); custom interceptors via `WithInterceptors()` option run after the built-ins; `WithShutdownTimeout` is the preferred alias of `WithTimeout`
- `grpc.RequestIDFrom(ctx)` extracts the restored request id (symmetric to `server/http.RequestIDFrom`)
- Health check service registered at `grpc.health.v1.Health`; poller runs checkers concurrently with per-check timeout (same `WithHealthCheckTimeout` as HTTP)

**EventBus** (eventbus/)
- 一等消息总线：`Bus` / `Topic[T]` / `Event[T]`；默认 `NewMemoryBus`，`app.Bus()` / Context / Default 解析
- 业务主路径：`Topic.Publish` / `Topic.Subscribe`（不必手传 Bus）
- 共享核心：`Resolver` 是 marshaler/retry/log-message/传播键解析与 Topic 级合并的唯一归属（contrib Bus 复用，`Bus.MarshalerFor` 委托它）；`InvokeHandler` 是订阅投递语义的唯一执行点（ctx 传播属性、固定退避重试、AutoAck/ContinueOnError 裁决；ack 时序留在适配器）。memory/watermill 不再各自复制查找链与重试循环
- `lynx.WithBusProvider(fn)` 配置驱动构造跨进程 Bus：框架装配好配置后调用 fn（cfg → bus + 配套 Services，如 kafka Transport 托管生命周期），是 watermill `NewFromConfig` 的推荐注入路径；已有现成实例仍用 `lynx.WithBus(bus)`（显式实例优先）

**Watermill Bus** (contrib/watermill/)
- Watermill Router 驱动的 `eventbus.Bus`；`lynx.*` 生命周期强制内存 Transport
- 投递语义（重试/AutoAck/ContinueOnError）委托 `eventbus.InvokeHandler`；ack 时序与 Nack 映射留在本适配器（AutoAck 先 Ack，WK-13）
- `NewFromConfig(cfg, transports)` 从 `bus` 段加载 topics/route；标识 `memory` 兼作 DefaultTransport
- 消费组语义：同 topic 多 handler 共用同组（含空 group 的 Transport 默认组）会被 `Subscribe` 拒绝——Kafka 组内瓜分分区是静默半量丢消息；广播用不同 group（`WithGroup` / topic group），竞争消费用单 handler + instances；内存 Transport 广播不受限
- 毒消息止损：`bus.max_redeliveries`（默认 10，主题级可覆盖）限制终态失败后的累计重投轮数，超过即 Ack 丢弃并记 Error
- Transports / DefaultTransport 生命周期独立于 Bus：需 Register 托管，`Bus.Stop` 不关闭它们

**Kafka Transport** (contrib/watermill-kafka/transport.go)
- 配置驱动：UnmarshalKey("kafka") 加载 map[逻辑topic] 配置（brokers/topics/consumer/producer）
- Init 即离线预构建并 `cfg.Validate()` 两侧 sarama 配置（非法 SASL 机制/压缩/初始 offset/CA 路径启动期报错）
- 内部按 brokers 分组客户端，订阅按（组 × 物理 topic × 实例数，上限 64 超出钳制+Warn）展开后 fan-in
- 基于 watermill-kafka/v3（IBM/sarama）；Kafka record Key = MessageKey / Event.Key
- `Subscribe` 返回 `eventbus.Delivery`（`Event` + Ack/Nack），Bus 转达到底层消息确认
- 同集群（brokers 相同）多 topic 配置差异经指纹比对 Warn（先构建者生效）；`log_message` 为 Debug 级（`--log-level=debug`）

**Scheduler** (contrib/schedule/scheduler.go)
- Cron-based task scheduling using robfig/cron
- Tasks implement `Task` interface with Name(), Cron(), HandlerFunc()
- `Start` respects the passed ctx (waits `<-ctx.Done()`); `Stop` is safe before Start (atomic `stopping` flag)
- Multi-node: `schedule.Exclusive(task)` + `WithCoordinator(cluster.Coordinator)` (per-fire TryOnce), or wrap the scheduler with `cluster.Singleton`

**Cluster** (contrib/cluster)
- `Coordinator.Claim` (one-shot occupancy, TTL expiry, no release) and `Coordinator.Acquire` (renewed lease)
- Claim/Acquire 骨架唯一归属 `cluster/lease.go`：`ValidateCall`（公共入参校验）、`RenewInterval`/`RunRenewLoop`（ttl/3 续约循环）供全部适配器共用；TTL 下限经可选能力 `cluster.TTLAware`/`cluster.MinTTL` 查询（Consul 10s，内存/Redis 无下限），schedule 的 Exclusive 触发会自动钳制
- Recipes: `TryOnce`, `Campaign`, `Singleton(lynx.Service)`
- Adapters: Memory; Consul Session+KV (`ttl >= 10s`, 声明 MinTTL); Redis SET NX (`contrib/cluster-redis`)

**Command** (command.go)
- CLI command execution with dependency readiness wait
- Readiness resolution shares ready.go's single three-tier module with OrderedServices (`probeServiceReady`): `Ready()` channel (bounded per-probe wait; sibling failure interrupts the run group and aborts the wait promptly) → `Checker` single bounded check (default 3s, `WithProbeTimeout`; a hung checker cannot stall the wait loop) → no signal = ready on invoke. MaxTries/WithBackoff remain the total budget
- Auto-closes application after command completes

### Health Checks

Services implementing `lynx.Checker` interface are automatically registered in the health check endpoint. HTTP server exposes these at `/healthz/liveness` and `/healthz/readiness`, gRPC server uses `grpc.health.v1.Health`.

### Application Entry Point

The `lynx.NewRunner()` function creates a `*Runner` instance with two run methods:
- `cli.Run()` - Panics on error
- `cli.RunE()` - Returns error for handling

## Code Style

- Uses EditorConfig: Go files use tabs, 4-space indent
- Unit tests exist for core packages and most contrib modules; run `go test -race ./...` per module
- Uses slog for structured logging (Go 1.24+)
- Uses root-level `errors.go`: `ShutdownErrors` shutdown-error aggregator and sentinel errors (`ErrNotInitialized`/`ErrSetupFuncNil`)
- Services obtain loggers via `ctx.Logger(...)` in `Init`; no external logging package

## Common Patterns

**Adding a New Service**
1. Implement the Service interface
2. Optionally implement lynx.Checker and/or lynx.Ready (Listen-based servers should close Ready after bind)
3. Register via `app.Register(myService)`, or sequence dependents with `app.Register(lynx.OrderedServices("name", a, b))`

**Adding a Hook**
```go
app.OnPreStart(func(ctx context.Context) error { ... })   // before services start
app.OnPostStart(func(ctx context.Context) error { ... })  // after all Starts invoked
app.OnDrain(func(ctx context.Context) error { ... })      // drain window (deregister)
app.OnPreStop(func(ctx context.Context) error { ... })    // before services stop
app.OnPostStop(func() { ... })                            // final cleanup (wire cleanup / close pools)
```

**Using Wire for DI**
1. Create provider functions returning dependencies
2. Add `//go:generate wire` and `//go:build wireinject` tags
3. Define injector function with wire.Build(ProviderSet)
4. Run `wire` or `go generate` to generate wire_gen.go

**Accessing Configuration**
```go
config := &MyConfig{}
app.Config().Unmarshal(config)
// or
value := app.Config().GetString("key")
```

**Context Utilities**
The framework provides a context helper to access app metadata:
- `lynx.Meta(ctx)` returns `lynx.Metadata{Name, ID, Version}` (empty fields when unset)
