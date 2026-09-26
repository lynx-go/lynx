# Lynx

[![Go Version](https://img.shields.io/badge/Go-1.26.5+-00ADD8?logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Lynx 是一个轻量级 Go 微服务框架：统一的应用生命周期、`Service` 插件化架构，以及 HTTP/gRPC、EventBus、配置、健康检查与可选的注册发现 / 可观测性扩展。

## 特性

- **生命周期** — 启动 / 排水（Drain）/ 优雅关闭；`OnPreStart` / `OnPostStart` / `OnDrain` / `OnPreStop` / `OnPostStop` 钩子
- **服务系统** — `Service` + `ServiceFactory`；实现 `Checker` 的服务自动进入健康检查
- **HTTP / gRPC** — 内置服务器与客户端（otel、请求日志、健康端点、中间件；默认 `request_id` / `user_id` 传播，HTTP 头与 gRPC metadata 同源）
- **EventBus** — 一等 `Bus` / `Topic[T]` / `Event[T]`，默认内存开箱即用
- **跨进程消息** — `contrib/watermill` Bus + `contrib/watermill-kafka` Transport
- **配置** — `Config` / `ConfigSource` 与具体库解耦，默认适配 Viper
- **可观测性** — OpenTelemetry（`contrib/telemetry`）、pprof（`debug`）
- **可测试性** — `lynxtest` 三层测试套件（组装测试 / 服务级 AppContext / 拨号辅助）
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
- 服务端默认安装 `request_id` / `user_id` 传播：合法的 `x-request-id` / `x-user-id`
  还原进请求 ctx 并回写响应头（`WithDisableRequestID()` 关闭）

更多完整示例见 [`_examples/http`](./_examples/http)。

### 配置

默认已绑定常用 flags（可用 `WithDisableConfigFlags()` 关闭；子命令框架等外部解析场景用 `WithConfigFile(path)` 绑定路径并关闭默认解析）：

```bash
-c, --config string       配置文件路径
    --config-type string  文件类型（默认 yaml）
    --config-dir string   配置目录
    --log-level string    日志级别
```

应用元数据键：`service.name` / `service.id` / `service.version`。  
日志级别：`logging.level` 为规范键（`log-level`/`log_level` 为仅配置文件的兼容回退，已废弃）；`--log-level` 显式传参时覆盖配置，自定义级别 flag 须在 `WithBindConfigFunc` 里翻译进规范键（见 `_examples/boot`）。

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

原始字节 / 原始信封：`Topic.Publish` 的 `[]byte` 与 `*RawEvent` payload 按透传处理（跳过序列化、保留信封），或 `app.Bus().Publish`。
框架生命周期事件（`lynx.*`）始终走进程内内存 Transport。

完整演示：[`_examples/bus`](./_examples/bus)。设计说明：[docs/design-eventbus.md](./docs/design-eventbus.md)。

### Watermill Bus + Kafka

跨进程时用配置装配 Watermill Bus（`bus:` + `kafka:` 段）。总线依赖配置，
经 `WithBusProvider` 在框架装配好配置后构造（kafka 版装配入口
`wmkafka.NewBusFromConfig`）——不必在 `NewRunner` 之前自行读配置；返回的
Transport 由框架托管生命周期：

```go
import (
	"github.com/lynx-go/lynx"
	wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"
)

lynx.NewRunner(setup,
	lynx.WithName("my-app"),
	lynx.WithBusProvider(wmkafka.NewBusFromConfig),
).Run()
```

```yaml
bus:
  max_redeliveries: 10   # 毒消息累计重投上限，超过后记 Error 并丢弃（默认 10）
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

Kafka 消费语义要点：

- **同一事件多 handler 并行扇出**：订阅单元是事件（逻辑 topic）——同一
  逻辑 topic 上多个 handler 共享一条 transport 订阅并进程内并行触发，
  每个 handler 都收到每条消息。消费组 / 消费者成员数是 kafka 配置
  （`kafka.<key>.consumer.group_id` / `.instances`），多实例部署按该组
  竞争消费。进程内在途上限由 `bus.topics.<topic>.max_in_flight` 控制
  （默认 1，串行且保序；调大并发，goroutine 有界）；挂死 handler 用
  `bus.handler_timeout` 止损。不同逻辑 topic 路由到同一物理 topic 且组
  相同时仍会互相瓜分，部署时应拆成不同 kafka 条目。
- **Transport 生命周期独立于 Bus**：`bus.Stop()` 不关闭 `opts.Transports`
  / `DefaultTransport`；Kafka Transport 必须作为独立服务 Register 交由
  框架托管 Start/Stop，漏注册则永远不会关闭。
- `log_message: true` 输出的是 **Debug 级**日志，需 `--log-level=debug`
  （或 `bus.debug: true`）开启，并非 Info 级。
- 毒消息止损：handler 终态失败（重试耗尽）后 Transport 会重投失败消息
  （Kafka 默认 100ms 一轮）；`bus.max_redeliveries`（默认 10，主题级
  `bus.topics.<topic>.max_redeliveries` 可覆盖）限制同一条消息的累计
  重投轮数，超过后记 Error 并丢弃该消息。

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

`Start` 通常阻塞至关停（如 `Serve`）；若只有非阻塞启动动作（拉起后台 goroutine、注册回调），用 `lynx.WaitForShutdown(ctx)` 收尾——`Start` 返回会立即触发整个应用关停。

注册须在 `Run()` 与 `Close()` 之前；`Run()` 开始后或 `Close()` 之后
`Register` / `RegisterFactory` 会 panic、`Command` 返回错误。

### 钩子与排水

```go
app.OnPreStart(func(ctx context.Context) error { /* 服务启动前：迁移/预热 */ return nil })
app.OnPostStart(func(ctx context.Context) error { /* 所有服务 Start 已调用：运行通知 */ return nil })
app.OnDrain(func(ctx context.Context) error { /* 排水窗口：如从注册中心注销 */ return nil })
app.OnPreStop(func(ctx context.Context) error { /* 服务 Stop 前：最后冲刷 */ return nil })
app.OnPostStop(func() { /* 一切停止后：关闭 DB/Redis 连接池（Wire cleanup） */ })
```

`WithDrainTimeout` 开启排水窗口：就绪检查立即失败（`lynx.ErrDraining`），便于 LB 摘流；`OnDrain` 与排水睡眠并发执行，**窗口即钩子总预算**（`DrainTimeout=0` 时注册钩子会在启动期报 `ErrDrainHooksRequireDrainTimeout`）；`OnPostStop` 收尾钩子受 `WithCleanupTimeout`（默认 10s）约束。

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
├── lynxtest/             # 测试套件（Run / NewContext / 拨号辅助）
├── internal/             # serverkit（server 共享规则）、clock（时间源）
├── contrib/
│   ├── watermill/        # Watermill 驱动的 eventbus.Bus
│   ├── watermill-kafka/  # Kafka Transport
│   ├── registry/         # Registrar / Resolver / memory / DNS
│   ├── consul/           # Consul 后端（含 cluster.Coordinator）
│   ├── cluster/          # 进程间协调（Claim / Acquire / Singleton）
│   ├── cluster-redis/    # Redis cluster.Coordinator
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
github.com/lynx-go/lynx/contrib/cluster
github.com/lynx-go/lynx/contrib/cluster-redis
github.com/lynx-go/lynx/contrib/schedule
github.com/lynx-go/lynx/contrib/zap
```

多模块发布时需分别打 tag（主仓 `v{version}`，contrib 为 `contrib/<name>/{version}`）。详见 `RELEASE.md` / `mise run release-all -- vX.Y.Z "release vX.Y.Z"`。

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
