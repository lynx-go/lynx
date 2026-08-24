# Lynx

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Lynx 是一个轻量级的 Go 微服务框架，提供了开箱即用的应用生命周期管理、服务系统、HTTP 服务器、健康检查、配置管理和事件驱动等功能。

## 特性

- **应用生命周期管理** - 简洁的启动/停止流程，支持优雅关闭
- **服务系统** - 基于 `Service` 接口的插件化架构
- **HTTP 服务器** - 内置 HTTP 服务器，支持健康检查和请求日志
- **调试服务** - 开箱即用的 pprof 诊断端点（默认仅监听本机回环）
- **HTTP 客户端** - 内置客户端，otel 插装、日志属性传播（request_id/user_id）、超时与重试
- **gRPC 客户端** - 内置客户端，otel 插装、日志属性传播（metadata）、调用超时与 TLS
- **健康检查** - 集成健康检查机制，便于监控和服务发现
- **可观测性** - 集成 OpenTelemetry tracing/metrics 与 Prometheus
- **配置管理** - 基于 Viper 的灵活配置系统，支持多来源配置
- **事件驱动** - 内置 EventBus（`Bus` / `Topic[T]` / `Event[T]`），默认内存开箱即用
- **Kafka 集成** - `contrib/watermill` + `contrib/watermill-kafka` 提供跨进程 Transport
- **定时任务** - 基于 Cron 的调度器支持
- **日志集成** - 支持 `slog` 和 `zap` 日志库
- **服务注册发现** - 可选 `contrib/registry`（Registrar/Resolver/memory/DNS）与 `contrib/consul` 生产后端
- **CLI 模式** - 支持命令行工具开发
- **依赖注入** - 支持 Wire 依赖注入

## 安装

```bash
go get github.com/lynx-go/lynx
```

## 快速开始

### HTTP 服务示例

```go
package main

import (
    "context"
    "encoding/json"
    gohttp "net/http"

    "github.com/lynx-go/lynx"
    "github.com/lynx-go/lynx/server/http"
)

func main() {
    cli := lynx.NewRunner(func(app lynx.App) error {
        // 创建 HTTP 路由
        router := gohttp.NewServeMux()
        router.HandleFunc("/", func(w gohttp.ResponseWriter, r *gohttp.Request) {
            json.NewEncoder(w).Encode(map[string]string{
                "hello": "world",
                "app":   lynx.Meta(app.Context()).Name,
            })
        })

        // 注册 HTTP 服务器服务
        app.Register(http.NewServer(router,
            http.WithAddr(":8080"),
            http.WithHealthCheckers(app.HealthCheckers),
        ))
        return nil
    },
        lynx.WithName("my-app"),
        lynx.WithVersion("1.0.0"),
    )

    cli.Run()
}
```

运行（监听地址已在代码中用 `http.WithAddr(":8080")` 指定）：

```bash
go run main.go
```

访问 http://localhost:8080 查看结果，访问 http://localhost:8080/healthz/liveness 与 http://localhost:8080/healthz/readiness 查看健康状态。

### 使用配置文件

创建 `config.yaml`：

```yaml
addr: ":8080"
log_level: "debug"
```

代码中绑定配置：

```go
opts := lynx.NewOptions(
    lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
        f.StringP("config", "c", "config.yaml", "config file path")
    }),
    lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
        if cf, _ := f.GetString("config"); cf != "" {
            c.SetFile(cf)
        }
        return nil
    }),
)
```

### 使用事件驱动

**推荐路径**：定义 `Topic[T]`，用 `Publish` / `Subscribe`；默认内存 Bus 由框架注入，
无需手传 Bus（解析顺序：`eventbus.WithBus` → Context → Default）。

```go
import "github.com/lynx-go/lynx/eventbus"

var UserCreated = eventbus.NewTopic[User]("user.created")

// 订阅（Init 或 OnStart 中；ctx 已含 Bus）
err := UserCreated.Subscribe(ctx, "notify",
    func(ctx context.Context, e *eventbus.Event[User]) error {
        // e.Payload 已是 User
        return nil
    })

// 发布：业务对象自动 JSON 序列化
err = UserCreated.Publish(ctx, User{Name: "alice"},
    eventbus.WithMessageKey("alice"))
```

需要原始字节时用 `app.Bus().Publish` / `PublishRaw`，或 `Topic.PublishRaw`。

自定义序列化器：`eventbus.Options{Marshaler: myMarshaler}`（`lynx.WithBusOptions`）或
`TopicMarshalers` / `WithTopicMarshaler` 按 topic 覆盖。

### 使用 Watermill Bus + Kafka Transport

跨进程时注入 Watermill Bus，并用配置装配 `bus:` + `kafka:`：

```go
import (
    "github.com/lynx-go/lynx"
    "github.com/lynx-go/lynx/contrib/watermill"
    wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"
    "github.com/lynx-go/lynx/eventbus"
)

kafkaT, err := wmkafka.NewFromConfig(cfg) // nil = kafka 段未启用
memT := watermill.NewMemoryTransport()
transports := map[string]eventbus.Transport{"memory": memT}
if kafkaT != nil {
    transports["kafka"] = kafkaT
}
bus, err := watermill.NewFromConfig(cfg, transports)
// ...
runner := lynx.NewRunner(setup, lynx.WithBus(bus), lynx.WithName("my-app"))
```

配置示例：

```yaml
bus:
  topics:
    user.created:
      route: { transport: kafka, key: user.created }
kafka:
  user.created:
    brokers: ["127.0.0.1:9092"]
    topics: [user_created]
    consumer: { group_id: users, instances: 3 }
    producer: { log_message: true }
```

也可代码直接构造 Kafka Transport：

```go
kafkaT, err := wmkafka.NewTransport(wmkafka.Options{
    Topics: map[string]wmkafka.TopicOptions{
        "user.created": {
            Brokers: []string{"127.0.0.1:9092"},
            Topics:  []string{"user_created"},
            Consumer: &wmkafka.ConsumerOptions{GroupID: "users", Instances: 3},
            Producer: &wmkafka.ProducerOptions{LogMessage: true},
        },
    },
})
```

### 使用定时任务

```go
import "github.com/lynx-go/lynx/contrib/schedule"

// 实现任务接口
type MyTask struct{}

func (t *MyTask) Name() string { return "my-task" }
func (t *MyTask) Cron() string { return "0 */5 * * * *" } // 每5分钟执行（含秒字段，共6段）
func (t *MyTask) HandlerFunc() schedule.HandlerFunc {
    return func(ctx context.Context) error {
        fmt.Println("Task executed!")
        return nil
    }
}

scheduler, _ := schedule.NewScheduler([]schedule.Task{&MyTask{}})
app.Register(scheduler)
```

## 核心概念

### Service（服务）

所有可管理的功能单元都实现 `Service` 接口：

```go
type Service interface {
    Name() string
    Init(ctx AppContext) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

服务的 `Init` 接收 `lynx.AppContext`（`Context`/`Config`/`Logger`/`HealthCheckers`/`Close`），
不依赖完整的 `App` 接口。`Stop` 返回的错误与 OnStop 钩子错误一起由 `Run()` 统一上抛。

### Hooks（钩子）

支持在应用启动和停止时执行自定义逻辑：

```go
app.OnStart(func(ctx context.Context) error {
    // 启动时执行
    return nil
})
app.OnStop(func(ctx context.Context) error {
    // 停止时执行
    return nil
})
```

### 依赖注入

结合 Wire 使用：

```go
//go:generate wire
func InitializeApp() (*Bootstrap, error) {
    wire.Build(
        NewServer,
        NewDatabase,
        NewRepository,
        NewService,
        NewBootstrap,
    )
    return &Bootstrap{}, nil
}
```

## 项目结构

```
lynx/
├── boot/           # 应用引导和依赖注入
├── runner.go      # Runner 入口（NewRunner）
├── eventbus/       # EventBus（Bus / Topic / Event，默认内存）
├── client/         # HTTP/gRPC 客户端
│   ├── grpc/       # gRPC 客户端
│   └── http/       # HTTP 客户端
├── command.go      # CLI 命令服务
├── service.go      # 服务接口定义
├── contrib/        # 扩展服务
│   ├── watermill/       # Watermill 驱动的 eventbus.Bus
│   ├── watermill-kafka/ # Kafka Transport（eventbus.Transport）
│   ├── registry/   # 服务注册发现（Registrar/Resolver/memory/DNS）
│   ├── consul/     # Consul 注册发现后端
│   ├── schedule/   # 定时任务
│   ├── telemetry/  # OpenTelemetry 生命周期托管
│   └── zap/        # Zap 日志集成
├── debug/          # pprof 运维诊断服务
├── docs/           # 文档
├── errors.go       # 错误聚合
├── health.go       # 健康检查工具
├── hooks.go        # Hooks 机制
├── logging/        # 日志机制（trace/attrs 注入）
├── options.go      # 配置选项
├── server/         # 服务器实现
│   ├── grpc/       # gRPC 服务器
│   └── http/       # HTTP 服务器
└── _examples/      # 示例代码
    ├── boot/       # 依赖注入示例
    ├── bus/        # EventBus 示例
    ├── cli/        # CLI 示例
    ├── http/       # HTTP 服务示例
    ├── registry/   # 服务注册发现示例
    └── schedule/   # 定时任务示例
```

## 配置

Lynx 使用 Viper 进行配置管理，支持多种配置来源：

- 命令行参数
- 环境变量
- 配置文件（JSON/YAML/TOML）

配置通过统一的 `lynx.Config` 接口访问（`app.Config()`），绑定阶段使用其超集 `lynx.ConfigSource`——接口与具体配置库解耦，默认实现适配 `*viper.Viper`，可替换为其他配置库。

默认支持的命令行参数：

```bash
-c, --config string      配置文件路径
--config-type string     配置文件类型 (默认 "yaml")
--config-dir string      配置文件目录
--log-level string       日志级别 (默认空，回退配置键，缺省 info)
```

## 扩展模块

### Contrib 模块

Lynx 提供了多个可选的扩展模块：

```bash
# HTTP 服务器
github.com/lynx-go/lynx/server/http

# EventBus（核心，默认内存）
github.com/lynx-go/lynx/eventbus

# Watermill 驱动的 Bus
github.com/lynx-go/lynx/contrib/watermill

# Kafka Transport
github.com/lynx-go/lynx/contrib/watermill-kafka

# 可观测性（OpenTelemetry 托管）
github.com/lynx-go/lynx/contrib/telemetry

# 服务注册发现（类型、Registrar/Resolver、memory/DNS 后端）
github.com/lynx-go/lynx/contrib/registry

# Consul 注册发现生产后端
github.com/lynx-go/lynx/contrib/consul

# 定时任务
github.com/lynx-go/lynx/contrib/schedule

# Zap 日志
github.com/lynx-go/lynx/contrib/zap
```

## 依赖要求

- Go 1.26.5 或更高版本

## License

Apache License 2.0

## 贡献

欢迎提交 Issue 和 Pull Request！

## 相关链接

- [文档](./docs/01-introduction.md)
- [示例](https://github.com/lynx-go/lynx/tree/main/_examples)
