# 4. 服务系统

服务是 Lynx 应用的基本构建单元：HTTP/gRPC 服务器、消息 Broker、定时调度器，乃至一段需要在应用生命周期内运行的后台逻辑，都可以抽象为一个服务。本章介绍 `Service` 接口契约、`ServiceFactory` 与多实例机制、`Checker`/`CheckHealth` 扩展接口，并通过一个完整示例演示如何编写自定义服务，最后概览 `contrib/` 下的五个官方服务模块。

## 4.1 Service 接口契约

`Service` 接口定义在 `service.go`，由 `Name` 方法和内嵌的 `Lifecycle` 接口组成：

```go
type Lifecycle interface {
	Init(ctx AppContext) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type Service interface {
	Name() string
	Lifecycle
}
```

四个方法的契约如下：

- `Name() string`：服务名称，用于启动/停止日志中的标识。框架不检查唯一性，多个实例可以重名。
- `Init(ctx AppContext) error`：注册服务时（即 `app.Register(...)` 调用时）**同步**执行，用于初始化依赖——可以通过参数 `ctx` 访问 `ctx.Config()`、`ctx.Logger()`、`ctx.Context()` 等（`AppContext` 是 `App` 的窄化子集，见 3.6 节）。返回 error 不会在注册时立即返回，而是被记录为首个注册错误，由 `Run()` 统一返回导致启动失败。
- `Start(ctx context.Context) error`：`cli.Run()` 启动后，每个服务在 run group 中作为独立 actor **并发**调用。通常是阻塞式的（监听端口、消费消息），收到 `ctx` 取消时应返回。任何一个服务的 `Start` 返回（无论是否出错）都会触发整个应用的优雅关闭（见 3.1 节并发模型）。
- `Stop(ctx context.Context) error`：关闭阶段由 run group 的中断函数调用，用于释放资源；返回的错误由框架收集，随 `Run()` 上抛。注意框架是先调用 `Stop` 再取消服务 Context（见 3.1 节），因此 `Stop` 中不要等待 `ctx.Done()`；`Stop` 必须容忍先于 `Start` 被调用（Init 成功但 Start 未执行时，框架会逆序调用 Stop 做资源清理）。

注册服务通过 `app.Register` 完成：

```go
app.Register(myService)
```

每个通过 `app.Register` 注册的服务会获得一个独立的 Context（注册时创建），`Start` 和 `Stop` 收到的都是这个 Context。

## 4.2 ServiceFactory 与多实例

当同一类服务需要运行多个实例时（例如同一个 Kafka 消费组起 3 个 consumer），直接 new 多个服务会很啰嗦，`ServiceFactory` 为此而生（定义同样在 `service.go`）：

```go
type ServiceFactory interface {
	New() Service
	Options() FactoryOptions
}

type FactoryOptions struct {
	Instances int `json:"instances"` // 实例数
}
```

注册方式与服务类似，使用 `app.RegisterFactories`：

```go
app.RegisterFactories(myFactory)
```

框架对工厂的处理逻辑（`lynx.go` 的 `addServiceFactories`）：

1. 调用 `Options()` 获取构建选项，`Instances` 小于 1 时按 1 处理；
2. 循环调用 `Instances` 次 `New()`，每次得到一个**全新**的服务实例；
3. 把这些实例逐一走与 `app.Register` 相同的注册流程（各自独立 `Init`/独立 Context/独立 run group actor）。

也就是说，`Instances: 3` 等价于注册三个互不影响的服务实例，`New()` 必须每次返回新对象，各实例之间不应共享会互相干扰的状态。

`boot` 包的 Wire 引导流程（`boot.Bind`）会把聚合好的服务与工厂一次性注册进应用，用法见 `_examples/boot/provides.go`。

## 4.3 Checker 与健康检查扩展接口

很多服务（服务器、Broker、调度器）还需要对外报告"自己是否健康"。Lynx 内置了 `lynx.Checker` 接口（替代 gocloud.dev 的 `health.Checker`）：

```go
type Checker interface {
	CheckHealth() error // 返回 nil 表示健康，返回 error 表示不健康
}
```

`Checker` 是独立于 `Service` 的扩展接口：框架在注册每个服务时会做 `Checker` 类型断言（`lynx.go` 的 `addServices`），只要服务实现了 `CheckHealth() error`，就会被自动收集进应用的健康检查列表。这个列表通过 `app.HealthCheckers()` 暴露（返回快照切片）：

```go
HealthCheckers() []Checker
```

它有两个消费方：

- HTTP 服务器的就绪端点：传入 `http.WithHealthCheckers(app.HealthCheckers)`（方法值天然匹配 `lynx.HealthCheckersFunc` 签名）后，`/healthz/readiness` 会依次调用所有收集到的检查器，全部通过才返回 200（见 2.5 节）。
- `app.Command` 注册的命令：命令执行前会带退避重试地等待所有检查器就绪（`command.go`），保证 CLI 命令不会抢在依赖服务就绪之前运行。

框架内置服务中，`server/grpc` 的 Server、`contrib/pubsub` 的 Broker、`contrib/kafka` 的 Transport、`contrib/schedule` 的 Scheduler 都实现了 `CheckHealth`。典型的实现语义是：未 `Start` 前返回 error，`Start` 成功后返回 nil，`Stop` 后再次返回 error（以 `contrib/schedule` 为例）：

```go
func (s *Scheduler) CheckHealth() error {
	if s.cron == nil {
		return errors.New("scheduler not initialized")
	}
	if !s.started.Load() {
		return errors.New("scheduler not running")
	}
	return nil
}
```

如果只需要一个"可开关"的健康状态而不关心具体逻辑，可以内嵌框架提供的 `lynx.HealthChecker`，用 `SetHealthy(true/false)` 控制就绪状态（完整用法见 2.5 节）。

## 4.4 自定义服务编写指南

编写自定义服务的要点：

1. 实现 `Name/Init/Start/Stop` 四个方法，`Start` 一般阻塞在 `ctx.Done()` 上；
2. 需要多实例时再配一个实现 `New/Options` 的工厂，`New()` 每次返回新实例；
3. 需要参与就绪检查就实现 `CheckHealth() error`，或直接内嵌 `lynx.HealthChecker`；
4. 在 `setup` 回调中用 `app.Register`（或 `app.RegisterFactories` 注册工厂）挂载服务。

下面是一个完整可编译的示例：一个 worker 服务内嵌 `HealthChecker` 参与就绪检查，并通过工厂以 2 个实例运行：

```go
package main

import (
	"context"

	"github.com/lynx-go/lynx"
)

func main() {
	cli := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		app.RegisterFactories(NewWorkerFactory("worker", 2))
		return nil
	},
		lynx.WithName("custom-component"),
	)
	cli.Run()
}

// worker 是一个自定义服务：实现 Service 接口，并内嵌 HealthChecker，
// 注册后自动成为 /healthz/readiness 的检查项。
type worker struct {
	*lynx.HealthChecker
	name string
}

func (w *worker) Name() string { return w.name }

func (w *worker) Init(ctx lynx.AppContext) error {
	w.SetHealthy(true) // 也可以等 Start 中就绪后再置为健康
	return nil
}

func (w *worker) Start(ctx context.Context) error {
	<-ctx.Done() // 阻塞直到服务 Context 被取消（即 Stop 返回之后）
	return nil
}

func (w *worker) Stop(ctx context.Context) error {
	w.SetHealthy(false)
	return nil
}

// workerFactory 负责按指定实例数构建 worker。
type workerFactory struct {
	name      string
	instances int
}

func NewWorkerFactory(name string, instances int) lynx.ServiceFactory {
	return &workerFactory{name: name, instances: instances}
}

func (f *workerFactory) New() lynx.Service {
	// 每次调用都返回新实例，实例之间不共享状态
	return &worker{name: f.name, HealthChecker: &lynx.HealthChecker{}}
}

func (f *workerFactory) Options() lynx.FactoryOptions {
	return lynx.FactoryOptions{Instances: f.instances}
}

var _ lynx.ServiceFactory = (*workerFactory)(nil)
```

运行后通过日志可以看到两个 worker 实例各自经历了 `initializing component` → `starting component`；按 `Ctrl+C` 后各自收到 `Stop`。由于内嵌了 `HealthChecker`，两个实例都会被收集为就绪检查项。

## 4.5 contrib 模块概览

`contrib/` 下的五个模块是框架官方维护的服务，各自是独立的 Go module，按需引入：

```bash
go get github.com/lynx-go/lynx/contrib/pubsub
go get github.com/lynx-go/lynx/contrib/kafka
go get github.com/lynx-go/lynx/contrib/telemetry
go get github.com/lynx-go/lynx/contrib/schedule
go get github.com/lynx-go/lynx/contrib/zap
```

### pubsub：消息发布订阅（Broker/Router/Handler）

`contrib/pubsub` 基于 Watermill 提供进程内/跨进程的事件发布订阅。核心概念：

- `Broker`：事件总线门面，本身是 `Service` 服务，提供 `Publish`/`Subscribe`/`Route`。内部维护一张 topic → Transport 路由表：`Options.Transports` 中每个 Transport 通过 `Topics()` 声明自己承接的逻辑 topic，`Init` 时自动建表；`Route(topic, t)` 可显式覆盖自动路由；`RouteKey(topic, t, key)` 在覆盖的同时把 transport 侧主题名改为 `key`（业务逻辑名与后端主题名解耦，如 kafka 时 `key` 对应 kafka 段配置的逻辑 key，发布与订阅两侧都会按 `key` 调用 transport）；未命中的 topic 回退到 `DefaultTransport`（两者皆无则返回错误）。
- `Transport`：消息后端服务（kafka/内存），topic 参数一律是逻辑名，物理名解析在实现内部，见下文的 kafka 模块。
- `Router`：把一组 `Handler` 在 `Init` 期缓冲订阅到 Broker 的服务，无时序依赖。`Handler` 接口由 `EventName()`、`HandlerName()`、`NewEvent()`、`Handle(ctx, event)` 四个方法组成，公共 API 使用自有 `pubsub.Message` 类型（`ID`/`Key`/`Headers`/`Payload`），与底层 Watermill 解耦。绝大多数场景无需手动实现该接口——用 `NewHandler`/`NewRawHandler` 工厂构造（见下文）。
- `NewFromConfig`：配置驱动装配——`pubsub.NewFromConfig(cfg, transports)` 从配置 `pubsub` 段加载显式路由并逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错），非 nil 的传入 transports 参与自动路由，`memory` 标识（提供时）兼作默认回退；不创建任何 transport，返回 `Broker`，transports 由调用方创建并注册。`kafka.NewFromConfig(cfg)` 配套加载 `kafka` 段创建 Transport，段缺失/为空返回 `(nil, nil)`（未启用）——**返回 nil 时不得 Register**（框架对 nil 服务注册返回明确错误）。

Handler 的类型化工厂（对齐 `_examples/pubsub/handlers.go` 的写法）：

```go
// HelloEvent 是 hello 逻辑 topic 的业务事件类型。
type HelloEvent struct {
	Message string `json:"message"`
}

// helloHandler 通过 NewHandler 构造类型化 handler：payload 经 topic 的
// Marshaler 自动反序列化为 HelloEvent，元数据经 TypedMessage 信封可见。
func helloHandler() pubsub.Handler {
	return pubsub.NewHandler("hello", "helloHandler", func(ctx context.Context, event *pubsub.TypedMessage[HelloEvent]) error {
		slog.InfoContext(ctx, "hello event", "message", event.Payload.Message, "key", event.Key)
		return nil
	})
}

// notifyHandler 用 NewRawHandler 处理原始字节：payload 不被序列化器处理。
func notifyHandler() pubsub.Handler {
	return pubsub.NewRawHandler("notify", "notifyHandler", func(ctx context.Context, event *pubsub.Message) error {
		slog.InfoContext(ctx, "notify event", "payload", string(event.Payload))
		return nil
	})
}
```

`NewHandler[T]` 是免反射的泛型擦除惯用法：类型参数 `T` 只在构造期使用，运行期经 `NewEvent()` 返回的 `*TypedMessage[T]` 信封声明解码目标（实现 `MessageDecoder`），`Init` 时按 topic 解析一次 Marshaler（`MarshalerFor`），消息到达时零额外开销地解码；`T` 为 `[]byte` 时是恒等解码，语义等价 raw。需要自定义解码或按需构造事件时再手动实现 `Handler` 接口（`NewEvent` 返回 nil 表示原始消息语义）。

手工装配与注册（`_examples/pubsub/provides.go` 以 Wire 组合同一组构造函数）：

```go
kafkaT, err := kafka.NewFromConfig(app.Config()) // nil = kafka 段缺失，未启用
if err != nil {
	return err
}
memT := pubsub.NewMemoryTransport()
transports := map[string]pubsub.Transport{"memory": memT}
if kafkaT != nil {
	transports["kafka"] = kafkaT
}
broker, err := pubsub.NewFromConfig(app.Config(), transports)
if err != nil {
	return err
}
app.Register(memT)
if kafkaT != nil {
	app.Register(kafkaT)
}
app.Register(broker)
app.Register(pubsub.NewRouter(broker, []pubsub.Handler{helloHandler(), notifyHandler()}))
```

发布事件（类型化 payload 自动 JSON 序列化）：

```go
_ = broker.Publish(ctx, "hello", HelloEvent{Message: "hello"},
	pubsub.WithMessageKey(uuid.NewString()),
)
```

需要原始字节时用 `pubsub.MustJSONMessage(...)` 预构建 `*Message` 发布，与 `NewRawHandler` 订阅对称。

### kafka：Kafka 传输（Transport）

`contrib/kafka` 提供 `pubsub.Transport` 的 Kafka 实现，配置驱动：`kafka.NewFromConfig(cfg)` 把配置文件 `kafka` 段整表加载为 `kafka.Options`（`map[逻辑topic]TopicOptions`）并构造 Transport（`NewTransport` 仍可用代码直接构造）。`TopicOptions` 声明 brokers、订阅的物理 topics、consumer/producer 参数：`Consumer == nil` 表示该 topic 只发布，`Producer == nil` 表示只订阅。内部按 brokers 集合分组复用客户端；订阅按（消费组 × 物理 topic × 实例数）展开后 fan-in 到单一 channel（取自 `_examples/pubsub/main.go` 与 `config.yaml`）：

`config.yaml`：

```yaml
kafka:
  hello:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_hello]
    consumer:
      group_id: consumer_hello
      instances: 3
      log_message: true
      session_timeout: 45s
      heartbeat_interval: 5s
    producer:
      log_message: true
      required_acks: -1
      compression: gzip
```

加载与注册：

```go
kafkaT, err := kafka.NewFromConfig(app.Config()) // 段缺失/为空返回 nil，未启用
if err != nil {
	return err
}
if kafkaT != nil {
	app.Register(kafkaT)
}
```

也可以代码直接构造（不依赖配置文件）：

```go
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
```

`ConsumerOptions.GroupID` / `Instances` 是订阅的默认值，可用 `pubsub.WithGroup` / `pubsub.WithInstances` 在代码中覆盖（显式值优先）；两者皆空时 `Subscribe` 报错。`ConsumerOptions.Instances` 是该 topic 同消费组的并发消费者成员数，上例即为 `hello` 事件启动 3 个并发 consumer。

### schedule：定时任务（Scheduler/Task）

`contrib/schedule` 基于 robfig/cron 提供定时任务调度。`Task` 接口由 `Name()`、`Cron()`（cron 表达式，支持秒级和 `@every 5s` 这类描述符）、`HandlerFunc()` 组成。用法（取自 `_examples/schedule/main.go`）：

```go
scheduler, err := schedule.NewScheduler([]schedule.Task{task1}, schedule.WithLogger(app.Logger()))
if err != nil {
	return err
}
app.Register(scheduler)
return nil
```

Task 的实现：

```go
type task struct{}

func (t *task) Name() string { return "TaskExample" }
func (t *task) Cron() string { return "@every 5s" }
func (t *task) HandlerFunc() schedule.HandlerFunc {
	return func(ctx context.Context) error {
		slog.InfoContext(ctx, "task triggered")
		return nil
	}
}

var _ schedule.Task = new(task)
```

`Scheduler` 实现了 `CheckHealth`：任务 handler 中的 panic 会被 recover 并记录日志，不会中断调度器。

### telemetry：可观测性托管

`contrib/telemetry` 以服务形式托管 OpenTelemetry 生命周期：Init 创建 TracerProvider/MeterProvider 并设置为 otel 全局值（**有意的全局副作用**，包注释中有醒目声明），默认 trace exporter 为 noop（生产忘配 exporter 不会向 stdout 倒 trace；开发调试用 `telemetry.WithStdoutTrace()`），metric reader 默认 Prometheus；Stop 自动 flush 并 shutdown。Init 在未显式 `WithResource` 时自动以应用名构建 `service.name` 资源属性。用法（取自 `_examples/http/main.go`）：

```go
app.Register(telemetry.New())
```

### zap：日志集成

`contrib/zap` 把 zap 包装成 `*slog.Logger`，日志级别复用框架统一的 `lynx.LogLevelFromConfig` 解析（`logging.level` 优先，`log-level`/`log_level` 兼容回退，均未设置时默认 `info`）；并自动附加 `service_id`、`service_name`、`version` 三个字段。一行接入（取自 `_examples/pubsub/main.go`）：

```go
app.SetLogger(zap.MustNewLogger(app))
```

如果需要在退出前 flush 缓冲日志，可以改用 `NewSyncableLogger`，并用 `zap.SyncOnStop(logger)` 生成一个 `OnStop` 钩子注册进应用：

```go
logger, _ := zap.NewSyncableLogger(app)
app.OnStop(zap.SyncOnStop(logger))
```

## 4.6 下一步

- [第 5 章：服务器](./05-servers.md) - 学习框架内置 HTTP/gRPC 服务器服务的全部配置项与可观测性接入
