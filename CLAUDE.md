# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Lynx is a lightweight Go microservice framework built on Go 1.24.2+ that provides application lifecycle management, a component-based architecture, and integrations for HTTP servers, messaging (Kafka/PubSub), scheduling, and configuration management.

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
- contrib/schedule: `contrib/schedule/{version}`

### Module Structure

This is a Go workspace using `go.work`. The main modules are:
- `./` - Core lynx framework
- `./_examples` - Example applications
- `./contrib/zap` - Zap logger integration
- `./contrib/pubsub` - PubSub abstraction layer (uses Watermill)
- `./contrib/kafka` - Kafka binder/consumer/producer
- `./contrib/schedule` - Cron scheduler

Each contrib module has its own `go.mod` with local replace directives pointing to `../../` for the main lynx module.

## Architecture

### Core Abstractions

**Component System**
All managed units implement the `Component` interface (component.go:15-18):
```go
type Component interface {
    Name() string
    Init(app Lynx) error
    Start(ctx context.Context) error
    Stop(ctx context.Context)
}
```

Components are registered via `app.Hooks(lynx.Components(...))` and automatically managed through their lifecycle.

**ComponentBuilder**
For dynamic component creation with configurable instance counts (component.go:24-27):
```go
type ComponentBuilder interface {
    Build() Component
    Options() BuildOptions
}
```

**Hooks**
Lifecycle hooks registered via `app.Hooks()` (hooks.go):
- `OnStart` - Functions to execute on startup
- `OnStop` - Functions to execute on shutdown
- `Components` - Register components
- `ComponentBuilders` - Register component builders

**Application Lifecycle**
The main run loop (lynx.go:239-279) uses `oklog/run` to manage concurrent goroutines:
1. Executes OnStart hooks
2. Runs all components (each component gets its own goroutine)
3. Listens for shutdown signals (SIGTERM, SIGQUIT, SIGINT, SIGKILL)
4. On shutdown: runs OnStop hooks with timeout, stops all components

**Context Values**
The application context carries standard values (lynx.go:43-65):
- `NameFromContext(ctx)` - Application name
- `IDFromContext(ctx)` - Instance ID (hostname by default)
- `VersionFromContext(ctx)` - Application version

### Configuration System

Uses Viper for configuration with pflag for CLI argument parsing. Configuration flow:
1. `SetFlagsFunc` - Register CLI flags
2. `BindConfigFunc` - Bind flags to Viper, set config file paths
3. Flags are parsed, config file is read, env vars are bound

Default flags (lynx.go:140-145):
- `--config/-c` - Config file path
- `--config-type` - File type (yaml, json, etc.)
- `--config-dir` - Config directory
- `--log-level` - Log level

### Boot/Bootstrap Pattern

The `boot` package provides a structured way to organize application initialization using Wire dependency injection:

1. Create provider functions for dependencies (see _examples/boot/provides.go)
2. Define a Wire injector function with `//go:build wireinject` tag
3. Register providers in a ProviderSet
4. Wire generates the dependency graph
5. Bootstrap.Build() registers all hooks/components with the app

This pattern is particularly useful for complex applications with many components.

### Key Components

**HTTP Server** (server/http/server.go)
- Wraps `gocloud.dev/server` with health check integration
- Support for request logging and custom timeouts
- Automatically registers health check endpoint at `/health`

**PubSub** (contrib/pubsub/)
- Abstraction over Watermill message library
- `Broker` interface provides Publish/Subscribe
- `Binder` interface for event-to-topic mapping
- Message context utilities for tracking message ID and keys

**Kafka Binder** (contrib/kafka/binder.go)
- Maps event names to Kafka topics
- Creates consumers and producers from configuration
- Uses PubSub broker abstraction internally
- Pattern: subscribe to internal topic, produce to external Kafka

**Scheduler** (contrib/schedule/scheduler.go)
- Cron-based task scheduling using robfig/cron
- Tasks implement `Task` interface with Name(), Cron(), HandlerFunc()
- Runs tasks in goroutines with context

**Command** (command.go)
- CLI command execution with health check dependency
- Retries waiting for components to be healthy before executing
- Auto-closes application after command completes

### Health Checks

Components implementing `health.Checker` interface are automatically registered in the health check endpoint. HTTP server exposes these at `/health`.

## Code Style

- Uses EditorConfig: Go files use tabs, 4-space indent
- No unit tests currently exist in the codebase
- Uses slog for structured logging (Go 1.24+)
- Error handling uses `github.com/pkg/errors`
- Uses panic-based fatal error handling via `pkg/errors` package

## Common Patterns

**Adding a New Component**
1. Implement the Component interface
2. Optionally implement health.Checker
3. Register via `app.Hooks(lynx.Components(myComponent))`

**Adding a Hook**
```go
app.Hooks(
    lynx.OnStart(func(ctx context.Context) error { ... }),
    lynx.OnStop(func(ctx context.Context) error { ... }),
)
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
