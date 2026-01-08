# Lynx

[![Go Version](https://img.shields.io/badge/Go-1.24.2+-00ADD8?logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Lynx 是一个轻量级的 Go 微服务框架，提供了开箱即用的应用生命周期管理、组件系统、HTTP 服务器、健康检查、配置管理和事件驱动等功能。

## 特性

- **应用生命周期管理** - 简洁的启动/停止流程，支持优雅关闭
- **组件系统** - 基于 `Component` 接口的插件化架构
- **HTTP 服务器** - 内置 HTTP 服务器，支持健康检查和请求日志
- **健康检查** - 集成健康检查机制，便于监控和服务发现
- **配置管理** - 基于 Viper 的灵活配置系统，支持多来源配置
- **事件驱动** - 内置 PubSub 支持，轻松实现异步消息处理
- **Kafka 集成** - 提供 Kafka Binder，简化消息队列的使用
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
    opts := lynx.NewOptions(
        lynx.WithName("my-app"),
        lynx.WithVersion("1.0.0"),
    )

    cli := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
        // 创建 HTTP 路由
        router := http.NewRouter()
        router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
            json.NewEncoder(w).Encode(map[string]string{
                "hello": "world",
                "app":   lynx.NameFromContext(app.Context()),
            })
        })

        // 注册 HTTP 服务器组件
        return app.Hooks(lynx.Components(
            http.NewServer(router,
                http.WithAddr(":8080"),
                http.WithHealthCheck(app.HealthCheckFunc()),
            ),
        ))
    })

    cli.Run()
}
```

运行：

```bash
go run main.go --addr=:8080
```

访问 http://localhost:8080 查看结果，访问 http://localhost:8080/health 查看健康状态。

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
    lynx.WithBindConfigFunc(func(f *pflag.FlagSet, v *viper.Viper) error {
        if c, _ := f.GetString("config"); c != "" {
            v.SetConfigFile(c)
        }
        return nil
    }),
)
```

### 使用事件驱动

```go
import "github.com/lynx-go/lynx/contrib/pubsub"

// 定义事件处理器
handler := pubsub.HandlerFunc(func(ctx context.Context, msg *message.Message) error {
    fmt.Printf("Received message: %s\n", msg.Payload)
    return nil
})

// 订阅主题
broker.Subscribe("user.created", "handler-1", handler)

// 发布消息
msg := pubsub.NewJSONMessage(map[string]string{"user": "alice"})
broker.Publish(ctx, "user.created", msg)
```

### 使用 Kafka Binder

```go
import "github.com/lynx-go/lynx/contrib/kafka"

binder := kafka.NewBinder(kafka.BinderOptions{
    SubscribeOptions: map[string]kafka.ConsumerOptions{
        "user.created": {
            Brokers: []string{"localhost:9092"},
            GroupID: "my-group",
        },
    },
    PublishOptions: map[string]kafka.ProducerOptions{
        "user.created": {
            Brokers: []string{"localhost:9092"},
        },
    },
})

app.Hooks(lynx.ComponentBuilders(binder.ConsumerBuilders()...))
app.Hooks(lynx.Components(binder))
```

### 使用定时任务

```go
import "github.com/lynx-go/lynx/contrib/schedule"

// 实现任务接口
type MyTask struct{}

func (t *MyTask) Name() string { return "my-task" }
func (t *MyTask) Cron() string { return "*/5 * * * *" } // 每5分钟执行
func (t *MyTask) HandlerFunc() schedule.HandlerFunc {
    return func(ctx context.Context) error {
        fmt.Println("Task executed!")
        return nil
    }
}

scheduler, _ := schedule.NewScheduler([]schedule.Task{&MyTask{}})
app.Hooks(lynx.Components(scheduler))
```

## 核心概念

### Component（组件）

所有可管理的功能单元都实现 `Component` 接口：

```go
type Component interface {
    Name() string
    Init(app Lynx) error
    Start(ctx context.Context) error
    Stop(ctx context.Context)
}
```

### Hooks（钩子）

支持在应用启动和停止时执行自定义逻辑：

```go
app.Hooks(
    lynx.OnStart(func(ctx context.Context) error {
        // 启动时执行
        return nil
    }),
    lynx.OnStop(func(ctx context.Context) error {
        // 停止时执行
        return nil
    }),
)
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
├── cli/            # CLI 模式支持
├── command/        # 命令行命令
├── component.go    # 组件接口定义
├── contrib/        # 扩展组件
│   ├── kafka/      # Kafka 支持
│   ├── pubsub/     # 消息发布订阅
│   ├── schedule/   # 定时任务
│   └── zap/        # Zap 日志集成
├── hooks.go        # Hooks 机制
├── options.go      # 配置选项
├── pkg/            # 内部工具包
├── server/         # 服务器实现
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

# PubSub 抽象
github.com/lynx-go/lynx/contrib/pubsub

# 定时任务
github.com/lynx-go/lynx/contrib/schedule

# Zap 日志
github.com/lynx-go/lynx/contrib/zap
```

## 依赖要求

- Go 1.24.2 或更高版本

## License

MIT License

## 贡献

欢迎提交 Issue 和 Pull Request！

## 相关链接

- [文档](https://github.com/lynx-go/lynx/wiki)
- [示例](https://github.com/lynx-go/lynx/tree/main/_examples)
