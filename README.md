# Lynx

[![Go Version](https://img.shields.io/badge/Go-1.26.5+-00ADD8?logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Lynx 是一个轻量级 Go 微服务框架：统一的应用生命周期、`Service` 插件化架构，以及 HTTP/gRPC、EventBus、配置、健康检查与可选的注册发现 / 可观测性扩展。

## 特性

- **生命周期** — 启动 / 排水（Drain）/ 优雅关闭；`OnStart` / `OnDrain` / `OnStop` 钩子
- **服务系统** — `Service` + `ServiceFactory`；实现 `Checker` 的服务自动进入健康检查
- **HTTP / gRPC** — 内置服务器与客户端（otel、请求日志、健康端点、中间件）
- **EventBus** — 一等 `Bus` / `Topic[T]` / `Event[T]`，默认内存开箱即用
- **跨进程消息** — `contrib/watermill` Bus + `contrib/watermill-kafka` Transport
- **配置** — `Config` / `ConfigSource` 与具体库解耦，默认适配 Viper
- **可观测性** — OpenTelemetry（`contrib/telemetry`）、pprof（`debug`）
- **扩展** — 注册发现（`registry` / `consul`）、Cron（`schedule`）、Zap（`zap`）、CLI、Wire

## 安装

要求 **Go 1.26.5+**：

```bash
go get github.com/lynx-go/lynx
```

## 快速开始

### HTTP 服务

```go
package main

import (
	"encoding/json"
	gohttp "net/http"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/http"
)

func main() {
	lynx.NewRunner(func(app lynx.App) error {
		mux := gohttp.NewServeMux()
		mux.HandleFunc("/", func(w gohttp.ResponseWriter, r *gohttp.Request) {
			meta := lynx.Meta(app.Context())
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hello": "world",
				"from":  meta.Name,
				"id":    meta.ID,
			})
		})
		app.Register(http.NewServer(mux,
			http.WithAddr(":8080"),
			http.WithHealthCheckers(app.HealthCheckers),
		))
		return nil
	},
		lynx.WithName("my-app"),
		lynx.WithVersion("1.0.0"),
	).Run()
}
```

```bash
go run main.go
```

- 业务：http://localhost:8080
- 存活 / 就绪：`/healthz/liveness`、`/healthz/readiness`

更多完整示例见 [`_examples/http`](./_examples/http)。

### 配置

默认已绑定常用 flags（可用 `WithDisableConfigFlags()` 关闭）：

```bash
-c, --config string       配置文件路径
    --config-type string  文件类型（默认 yaml）
    --config-dir string   配置目录
    --log-level string    日志级别
```

应用元数据键：`service.name` / `service.id` / `service.version`。  
日志级别回退：`logging.level` → `log-level` → `log_level`。

通过 `app.Config()` 读取（`Get` / 类型化 getter / `Unmarshal`）。绑定阶段使用 `ConfigSource`（`Set` / `SetFile` / `BindEnv` 等）；默认实现适配 `*viper.Viper`。

### EventBus

默认内存 Bus 由框架注入，无需 `Register`。业务以 `Topic[T]` 为主路径；Bus 解析顺序：`eventbus.WithBus` → Context → `Default()`。

```go
import "github.com/lynx-go/lynx/eventbus"

var UserCreated = eventbus.NewTopic[User]("user.created")

// 订阅（Init / OnStart；handler 名用 Option，省略时默认为 topic）
err := UserCreated.Subscribe(ctx,
	func(ctx context.Context, e *eventbus.Event[User]) error {
		// e.Payload 已是 User
		return nil
	}, eventbus.WithHandlerName("notify"))

// 发布（自动 JSON 序列化）
err = UserCreated.Publish(ctx, User{Name: "alice"},
	eventbus.WithMessageKey("alice"))
```

原始字节：`Topic.PublishRaw` 或 `app.Bus().Publish` / `PublishRaw`。  
框架生命周期事件（`lynx.*`）始终走进程内内存 Transport。

完整演示：[`_examples/bus`](./_examples/bus)。设计说明：[docs/design-eventbus.md](./docs/design-eventbus.md)。

### Watermill Bus + Kafka

跨进程时注入 Watermill Bus，用配置装配 `bus:` + `kafka:`：

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

lynx.NewRunner(setup, lynx.WithBus(bus), lynx.WithName("my-app")).Run()
```

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

Kafka record Key = `Event.Key` / MessageKey。Transport `Subscribe` 返回 `Delivery`（`Ack` / `Nack`），由 Bus 转达底层确认。

### 定时任务

```go
import "github.com/lynx-go/lynx/contrib/schedule"

type MyTask struct{}

func (t *MyTask) Name() string { return "my-task" }
func (t *MyTask) Cron() string { return "0 */5 * * * *" } // 6 段，含秒
func (t *MyTask) HandlerFunc() schedule.HandlerFunc {
	return func(ctx context.Context) error { return nil }
}

scheduler, _ := schedule.NewScheduler([]schedule.Task{&MyTask{}})
app.Register(scheduler)
```

## 核心概念

### Service

```go
type Service interface {
	Name() string
	Init(ctx AppContext) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}
```

`Init` 接收 `AppContext`（`Context` / `Config` / `Logger` / `HealthCheckers` / `Bus` / `Close`），不依赖完整 `App`。`Stop` 与钩子错误由 `Run()` 聚合上抛。

注册须在 `Run()` 之前；`Run()` 开始后 `Register` / `RegisterFactories` 会 panic。

### 钩子与排水

```go
app.OnStart(func(ctx context.Context) error { return nil })
app.OnDrain(func(ctx context.Context) error { /* 如从注册中心注销 */ return nil })
app.OnStop(func(ctx context.Context) error { return nil })
```

`WithDrainTimeout` 开启排水窗口：就绪检查立即失败（`lynx.ErrDraining`），便于 LB 摘流；`OnDrain` 与排水睡眠并发，受 `WithDrainHookTimeout`（默认 3s）约束。

### Wire

```go
//go:generate wire
func InitializeApp() (*Bootstrap, error) {
	wire.Build(ProviderSet)
	return nil, nil
}
```

见 [`_examples/boot`](./_examples/boot)。

## 项目结构

```
lynx/
├── boot/                 # Bootstrap + Wire
├── eventbus/             # Bus / Topic / Event（默认内存）
├── client/{http,grpc}/   # 出站客户端
├── server/{http,grpc}/   # 入站服务器
├── debug/                # pprof（默认本机回环）
├── logging/              # slog 属性 / trace 注入
├── contrib/
│   ├── watermill/        # Watermill 驱动的 eventbus.Bus
│   ├── watermill-kafka/  # Kafka Transport
│   ├── registry/         # Registrar / Resolver / memory / DNS
│   ├── consul/           # Consul 后端
│   ├── schedule/         # Cron
│   ├── telemetry/        # OpenTelemetry 生命周期
│   └── zap/              # Zap 日志
├── docs/                 # 文档与设计稿
└── _examples/            # boot / bus / cli / http / registry / schedule
```

## 扩展模块

```bash
github.com/lynx-go/lynx/server/http
github.com/lynx-go/lynx/server/grpc
github.com/lynx-go/lynx/eventbus
github.com/lynx-go/lynx/contrib/watermill
github.com/lynx-go/lynx/contrib/watermill-kafka
github.com/lynx-go/lynx/contrib/telemetry
github.com/lynx-go/lynx/contrib/registry
github.com/lynx-go/lynx/contrib/consul
github.com/lynx-go/lynx/contrib/schedule
github.com/lynx-go/lynx/contrib/zap
```

多模块发布时需分别打 tag（主仓 `v{version}`，contrib 为 `contrib/<name>/{version}`）。详见 Taskfile / `task release-all`。

## 文档与示例

| 资源 | 说明 |
|------|------|
| [docs/01-introduction.md](./docs/01-introduction.md) | 项目简介 |
| [docs/02-quick-start.md](./docs/02-quick-start.md) | 快速开始 |
| [docs/03-core-concepts.md](./docs/03-core-concepts.md) | 核心概念 |
| [docs/04-service-system.md](./docs/04-service-system.md) | 服务系统 |
| [docs/05-servers.md](./docs/05-servers.md) | HTTP / gRPC |
| [docs/06-clients.md](./docs/06-clients.md) | 客户端 |
| [docs/07-registry.md](./docs/07-registry.md) | 注册发现 |
| [docs/design-eventbus.md](./docs/design-eventbus.md) | EventBus 设计 |
| [_examples/](./_examples/) | 可运行示例 |

## License

Apache License 2.0

欢迎提交 Issue 与 Pull Request。
