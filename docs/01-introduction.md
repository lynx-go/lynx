# 1. 项目简介

## 1.1 什么是 Lynx

Lynx 是一个轻量级的 Go 微服务框架，旨在提供开箱即用的应用生命周期管理和组件化开发体验。它吸收了多个优秀开源项目的精华，提供了一个简洁、灵活且强大的开发框架。

## 1.2 设计理念

### 简洁至上
Lynx 遵循"简单即美"的设计哲学，提供最小化的核心 API，开发者可以快速上手并专注于业务逻辑的实现。

### 组件化架构
通过统一的 `Component` 接口，Lynx 将所有可管理的功能单元抽象为组件，实现了高度的模块化和可扩展性。

### 优雅的生命周期管理
内置完善的启动/停止流程，支持优雅关闭（Graceful Shutdown），确保服务在启停过程中不丢失数据和请求。

### 生产就绪
集成了健康检查、日志、配置管理等生产环境必需的功能，让开发者能够快速构建生产级别的微服务。

## 1.3 核心特性

### 应用生命周期管理
- 简洁的启动/停止流程
- 支持优雅关闭
- 钩子机制（Hooks）支持自定义生命周期逻辑

### 组件系统
- 基于 `Component` 接口的插件化架构
- 组件自动注册和管理
- 支持组件依赖关系

### 服务器支持
- **HTTP 服务器**：内置 HTTP 服务器，支持健康检查、请求日志
- **gRPC 服务器**：内置 gRPC 服务器，支持拦截器、健康检查、反射服务

### 健康检查
- 集成 gocloud.dev 健康检查机制
- 支持自定义健康检查函数
- 便于监控和服务发现集成

### 配置管理
- 基于 Viper 的灵活配置系统
- 支持多来源配置（命令行、环境变量、配置文件）
- 支持多种配置格式（JSON、YAML、TOML 等）

### 事件驱动
- 内置 PubSub 支持
- 支持异步消息处理
- 提供 Kafka Binder 简化消息队列使用

### 定时任务
- 基于 Cron 的调度器
- 支持任务注册和管理
- 灵活的任务执行控制

### 日志集成
- 支持 `log/slog` 标准库
- 集成 `zap` 高性能日志库
- 结构化日志支持

### CLI 模式
- 支持命令行工具开发
- 子命令管理
- 参数绑定

### 依赖注入
- 支持 Wire 依赖注入
- 自动生成依赖代码
- 编译时依赖检查

## 1.4 适用场景

Lynx 适用于以下场景：

### 微服务开发
快速构建和部署微服务，内置的服务器组件和健康检查机制让微服务开发变得简单。

### CLI 工具开发
构建命令行工具和后台服务，统一的 CLI 接口简化了命令行应用的开发。

### 事件驱动应用
构建基于事件的异步处理系统，内置的 PubSub 和 Kafka 支持让消息处理变得简单。

### 定时任务系统
开发需要定时执行任务的应用，内置的调度器支持灵活的任务管理。

## 1.5 技术栈

Lynx 构建于以下优秀的开源项目之上：

- **Go 1.24.2+**：编程语言
- **gocloud.dev**：云原生 API 支持
- **Viper**：配置管理
- **Cobra**：命令行框架
- **Wire**：依赖注入
- **gRPC**：RPC 框架
- **Zap**：高性能日志库
- **Cron**：定时任务调度

## 1.6 项目结构

```
lynx/
├── boot/           # 应用引导和依赖注入
├── cli/            # CLI 模式支持
├── command/        # 命令行命令
├── contrib/        # 扩展组件
│   ├── kafka/      # Kafka 支持
│   ├── pubsub/     # 消息发布订阅
│   ├── schedule/   # 定时任务
│   └── zap/        # Zap 日志集成
├── server/         # 服务器实现
│   ├── http/       # HTTP 服务器
│   └── grpc/       # gRPC 服务器
└── _examples/      # 示例代码
```

## 1.7 快速体验

### 安装

```bash
go get github.com/lynx-go/lynx
```

### 最简单的 HTTP 服务

```go
package main

import (
    "context"
    "net/http"

    "github.com/lynx-go/lynx"
    "github.com/lynx-go/lynx/server/http"
)

func main() {
    router := http.NewRouter()
    router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte("Hello, Lynx!"))
    })

    opts := lynx.NewOptions(
        lynx.WithName("my-app"),
        lynx.WithVersion("1.0.0"),
    )

    cli := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
        return app.Hooks(lynx.Components(
            http.NewServer(router, http.WithAddr(":8080")),
        ))
    })

    cli.Run()
}
```

运行：
```bash
go run main.go
```

访问 http://localhost:8080 即可看到结果。

## 1.8 下一步

- [第 2 章：快速开始](./02-quick-start.md) - 学习如何创建第一个 Lynx 应用
- [第 3 章：核心概念](./03-core-concepts.md) - 了解 Lynx 的核心设计理念
- [第 4 章：组件系统](./04-component-system.md) - 深入理解组件化架构
- [第 5 章：服务器](./05-servers.md) - 学习 HTTP 和 gRPC 服务器使用
- [示例代码](../_examples/) - 查看更多实际应用示例
