# Lynx

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Lynx 是一个轻量级的 Go 微服务框架，提供了开箱即用的应用生命周期管理、组件系统、HTTP 服务器、健康检查、配置管理和事件驱动等功能。

## 特性

- **应用生命周期管理** - 简洁的启动/停止流程，支持优雅关闭
- **组件系统** - 基于 `Component` 接口的插件化架构
- **HTTP 服务器** - 内置 HTTP 服务器，支持健康检查和请求日志
- **健康检查** - 集成健康检查机制，便于监控和服务发现
- **可观测性** - 集成 OpenTelemetry tracing/metrics 与 Prometheus
- **配置管理** - 基于 Viper 的灵活配置系统，支持多来源配置
- **事件驱动** - 内置 PubSub 支持，轻松实现异步消息处理
- **Kafka 集成** - 提供 Kafka Transport，简化消息队列的使用
- **定时任务** - 基于 Cron 的调度器支持
- **日志集成** - 支持 `slog` 和 `zap` 日志库
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
    "net/http"

    "github.com/lynx-go/lynx"
    "github.com/lynx-go/lynx/server/http"
)

func main() {
    cli := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
        // 创建 HTTP 路由
        router := http.NewRouter()
        router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
            json.NewEncoder(w).Encode(map[string]string{
                "hello": "world",
                "app":   lynx.NameFromContext(app.Context()),
            })
        })

        // 注册 HTTP 服务器组件
        app.Register(http.NewServer(router,
            http.WithAddr(":8080"),
            http.WithHealthCheck(app.HealthCheckFunc()),
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
    lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
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

**透明序列化（推荐）**：Publish 直接传业务对象，订阅用类型化 handler，
序列化由 Broker 的 marshaller（默认 JSON）自动处理：

```go
import "github.com/lynx-go/lynx/contrib/pubsub"

// 发布：业务对象自动 JSON 序列化
err := broker.Publish(ctx, "user.created", map[string]string{"user": "alice"},
    pubsub.WithMessageKey("alice"))

// 订阅：自动反序列化到指定类型
err := pubsub.Subscribe(ctx, broker, "user.created", "notify",
    func(ctx context.Context, event *pubsub.TypedMessage[User]) error {
        // event.Payload 已是 User 结构
        return nil
    })
```

需要原始字节时保留字节级语义（payload 传 `*pubsub.Message` 不序列化，
或直接用 `pubsub.MustJSONMessage` 预构建）：

```go
handler := pubsub.HandlerFunc(func(ctx context.Context, msg *pubsub.Message) error {
    // msg.ID / msg.Key / msg.Headers / msg.Payload
    return nil
})

msg := pubsub.MustJSONMessage(map[string]string{"user": "alice"})
err := broker.Publish(ctx, "user.created", msg, pubsub.WithMessageKey("alice"))
```

自定义序列化器（如 Protobuf）：`pubsub.NewBroker(pubsub.Options{Marshaler: myMarshaler})`；
按 topic 差异化（如 audit 用 Protobuf、其余 JSON）：
`pubsub.Options{Marshaler: jsonMarshaler, TopicMarshalers: map[string]pubsub.Marshaler{"audit": pbMarshaler}}`。

### 使用 Kafka Transport

```go
import "github.com/lynx-go/lynx/contrib/kafka"

// 从配置文件加载（config.yaml 的 kafka 段），或代码构造：
kafkaT, err := kafka.NewTransport(kafka.Options{
    Topics: map[string]kafka.TopicOptions{
        "user.created": {
            Brokers: []string{"127.0.0.1:9092"},
            Topics:  []string{"user_created"},
            Consumer: &kafka.ConsumerOptions{GroupID: "users", Instances: 3},
            Producer: &kafka.ProducerOptions{LogMessage: true},
        },
    },
})
broker := pubsub.NewBroker(pubsub.Options{
    Transports:       []pubsub.Transport{kafkaT},
    DefaultTransport: pubsub.NewMemoryTransport(),
})
app.Register(kafkaT, broker)
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

### Component（组件）

所有可管理的功能单元都实现 `Component` 接口：

```go
type Component interface {
    Name() string
    Init(app App) error
    Start(ctx context.Context) error
    Stop(ctx context.Context)
}
```

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
├── builder.go      # Builder 入口（NewBuilder）
├── command.go      # CLI 命令组件
├── component.go    # 组件接口定义
├── contrib/        # 扩展组件
│   ├── kafka/      # Kafka 支持
│   ├── metrics/    # OpenTelemetry 生命周期托管
│   ├── pubsub/     # 消息发布订阅
│   ├── schedule/   # 定时任务
│   └── zap/        # Zap 日志集成
├── docs/           # 文档
├── errors.go       # 错误聚合
├── health.go       # 健康检查工具
├── hooks.go        # Hooks 机制
├── logging.go      # trace 日志注入
├── options.go      # 配置选项
├── server/         # 服务器实现
│   ├── grpc/       # gRPC 服务器
│   └── http/       # HTTP 服务器
└── _examples/      # 示例代码
    ├── boot/       # 依赖注入示例
    ├── cli/        # CLI 示例
    ├── http/       # HTTP 服务示例
    ├── pubsub/     # 消息队列示例
    └── schedule/   # 定时任务示例
```

## 配置

Lynx 使用 Viper 进行配置管理，支持多种配置来源：

- 命令行参数
- 环境变量
- 配置文件（JSON/YAML/TOML）
- 远程配置中心

配置通过统一的 `lynx.Config` 接口访问（`app.Config()`），绑定阶段使用其超集 `lynx.ConfigSource`——接口与具体配置库解耦，默认实现适配 `*viper.Viper`，可替换为其他配置库。

默认支持的命令行参数：

```bash
-c, --config string      配置文件路径
--config-type string     配置文件类型 (默认 "yaml")
--config-dir string      配置文件目录
--log-level string       日志级别 (默认 "info")
```

## 扩展模块

### Contrib 模块

Lynx 提供了多个可选的扩展模块：

```bash
# HTTP 服务器
github.com/lynx-go/lynx/server/http

# Kafka 支持
github.com/lynx-go/lynx/contrib/kafka

# 可观测性（OpenTelemetry 托管）
github.com/lynx-go/lynx/contrib/metrics

# PubSub 抽象
github.com/lynx-go/lynx/contrib/pubsub

# 定时任务
github.com/lynx-go/lynx/contrib/schedule

# Zap 日志
github.com/lynx-go/lynx/contrib/zap
```

## 依赖要求

- Go 1.25 或更高版本

## License

Apache License 2.0

## 贡献

欢迎提交 Issue 和 Pull Request！

## 相关链接

- [文档](./docs/01-introduction.md)
- [示例](https://github.com/lynx-go/lynx/tree/main/_examples)
