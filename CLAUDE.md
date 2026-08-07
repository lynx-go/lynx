# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Lynx is a lightweight Go microservice framework built on Go 1.26+ that provides application lifecycle management, a service-based architecture, and integrations for HTTP servers, messaging (Kafka/PubSub), scheduling, and configuration management.

## Development Commands

### Building and Running

```bash
# Run examples
cd _examples/http && go run main.go --addr=:8080
cd _examples/cli && go run main.go
cd _examples/pubsub && go run main.go
cd _examples/schedule && go run main.go
cd _examples/boot && go run main.go

# Generate Wire dependency injection code
cd _examples/boot && wire
# Or use go generate
go generate ./...
```

### Release Management

Uses Task (taskfile) for releases:

```bash
# Release all modules at once (tags main repo and all contrib modules)
task release-all

# Individual module releases
task release-tag --Version=v0.5.8 --Comment="release message"
```

The project uses a multi-module release strategy. When releasing, you must tag:
- Main repo: `v{version}`
- contrib/zap: `contrib/zap/{version}`
- contrib/pubsub: `contrib/pubsub/{version}`
- contrib/kafka: `contrib/kafka/{version}`
- contrib/telemetry: `contrib/telemetry/{version}`
- contrib/schedule: `contrib/schedule/{version}`

### Module Structure

This is a Go workspace using `go.work`. The main modules are:
- `./` - Core lynx framework
- `./_examples` - Example applications
- `./contrib/zap` - Zap logger integration
- `./contrib/pubsub` - PubSub abstraction layer (uses Watermill)
- `./contrib/kafka` - Kafka Transport service (watermill-kafka/v3)
- `./contrib/telemetry` - OpenTelemetry lifecycle management (trace/metrics providers)
- `./contrib/schedule` - Cron scheduler

Server implementations (within main module):
- `./server/http` - HTTP server using stdlib `net/http` with otelhttp instrumentation
- `./server/grpc` - gRPC server with interceptors

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
    Close()
}

type Lifecycle interface {
    Init(ctx AppContext) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

Services are registered via `app.Register(...)` and automatically managed through their lifecycle. Services implementing `lynx.Checker` (`CheckHealth() error`, defined locally in health.go — no gocloud.dev dependency) are automatically added to health checks; `app.HealthCheckers()` returns the snapshot slice. `Stop` errors are collected (bounded by `Options.StopTimeout`) and surfaced by `Run()` together with OnStop hook errors.

**ServiceFactory**
For dynamic service creation with configurable instance counts (service.go:40-55):
```go
type ServiceFactory interface {
    New() Service
    Options() FactoryOptions
}
```

**Hooks & Registration**
Lifecycle hooks and services are registered via direct methods on the `App` interface (lynx.go):
- `app.OnStart(fns ...HookFunc)` - Functions to execute on startup
- `app.OnStop(fns ...HookFunc)` - Functions to execute on shutdown
- `app.Register(services ...Service)` - Register services (Init runs synchronously at registration; the first error is recorded and returned by `Run()`). All registration must happen before `Run()`: after `Run()` starts, `Register`/`RegisterFactories` panic and `Command` returns an error
- `app.RegisterFactories(factories ...ServiceFactory)` - Register service factories
- `app.Command(cmd CommandFunc)` - Register a one-shot CLI command

**Application Lifecycle**
The main run loop (lynx.go:497-572) uses `oklog/run` to manage concurrent goroutines:
1. Executes OnStart hooks
2. Runs all services (each service gets its own goroutine)
3. Listens for shutdown signals (SIGTERM, SIGQUIT, SIGINT)
4. On shutdown: runs OnStop hooks with timeout, stops all services

Optional drain window (`Options.DrainTimeout`, default 0 = disabled): on shutdown, an internal `drainChecker` is set so readiness aggregation (`app.HealthCheckers()`) fails immediately (LB 摘流), then the app sleeps `DrainTimeout` before cancelling the context and proceeding with the v1.0 shutdown sequence. DrainTimeout is a separate budget from ShutdownTimeout: total shutdown upper bound = DrainTimeout + ShutdownTimeout + StopTimeout stack. Drain only affects readiness (HTTP `/healthz/liveness` never consumes checkers).

**Context Values**
The application context carries standard values (lynx.go:65-105):
- `NameFromContext(ctx)` - Application name
- `IDFromContext(ctx)` - Instance ID (hostname by default)
- `VersionFromContext(ctx)` - Application version

### Configuration System

Configuration is exposed through two generic interfaces, decoupled from the underlying library (the default implementation adapts `*viper.Viper` via `lynx.NewViperConfig`):

- `lynx.Config` - read-only config access, returned by `app.Config()`: `Get(path)` (dot-separated paths), typed getters (`GetString`/`GetBool`/`GetInt`/`GetStringMap`/`GetStringSlice`), `IsSet`, `Unmarshal(out)`
- `lynx.ConfigSource` - superset of `Config`, received by `BindConfigFunc`: adds `Set`, `SetFile`, `AddSearchPath`, `SetFileFormat`, `SetEnvPrefix`, `AutomaticEnv`, `BindEnv`

Other config libraries (e.g. koanf) can be integrated by implementing these two interfaces.

Configuration flow:
1. `SetFlagsFunc` - Register CLI flags
2. `BindConfigFunc` - Bind flags to the app ConfigSource, set config file paths
3. Flags are parsed, config file is read, env vars are bound

Default flags are enabled by default (`Options.EnsureDefaults` sets `DefaultSetFlagsFunc`/`DefaultBindConfigFunc`); opt out with `WithDisableConfigFlags()`. Unknown flags are ignored (test binaries' `-test.*` args). `--help` returns an init error handled by `Runner.Run` exit code.

Default flags (see `DefaultSetFlagsFunc` in lynx.go):
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
5. Bootstrap.Bind(app) registers all hooks/services with the app

This pattern is particularly useful for complex applications with many services.

### Key Services

**HTTP Server** (server/http/server.go)
- Wraps stdlib `net/http.Server` with otelhttp instrumentation (health check handlers and request log are implemented locally — no gocloud.dev dependency)
- Support for request logging and custom timeouts
- Automatically registers health check endpoints at `/healthz/liveness` and `/healthz/readiness`

**gRPC Server** (server/grpc/server.go)
- Wraps `google.golang.org/grpc` with health check and reflection
- Built-in logging and recovery interceptors
- Custom interceptors via `WithInterceptors()` option
- Health check service registered at `grpc.health.v1.Health`

**PubSub** (contrib/pubsub/)
- Broker 门面服务：topic → Transport 路由表（自动路由 + 显式 Route + 默认回退）
- Transport 接口：后端即服务（kafka/内存），公共 API 使用自有 Message 类型
- Router 服务：Init 期缓冲注册 Handler 订阅，无时序依赖

**Kafka Transport** (contrib/kafka/transport.go)
- 配置驱动：UnmarshalKey("kafka") 加载 map[逻辑topic] 配置（brokers/topics/consumer/producer）
- 内部按 brokers 分组客户端，订阅按（组 × 物理 topic × 实例数）展开后 fan-in
- 基于 watermill-kafka/v3（IBM/sarama）

**Scheduler** (contrib/schedule/scheduler.go)
- Cron-based task scheduling using robfig/cron
- Tasks implement `Task` interface with Name(), Cron(), HandlerFunc()
- `Start` respects the passed ctx (waits `<-ctx.Done()`); `Stop` is safe before Start (atomic `stopping` flag)

**Command** (command.go)
- CLI command execution with health check dependency
- Retries waiting for services to be healthy before executing
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
2. Optionally implement lynx.Checker
3. Register via `app.Register(myService)`

**Adding a Hook**
```go
app.OnStart(func(ctx context.Context) error { ... })
app.OnStop(func(ctx context.Context) error { ... })
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
The framework provides context helpers to access app metadata:
- `lynx.NameFromContext(ctx)` - Get app name
- `lynx.IDFromContext(ctx)` - Get instance ID
- `lynx.VersionFromContext(ctx)` - Get app version
