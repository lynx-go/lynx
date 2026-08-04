# 4. 组件系统

组件是 Lynx 应用的基本构建单元：HTTP/gRPC 服务器、消息 Broker、定时调度器，乃至一段需要在应用生命周期内运行的后台逻辑，都可以抽象为一个组件。本章介绍 `Component` 接口契约、`ComponentBuilder` 与多实例机制、`ServerLike`/`CheckHealth` 扩展接口，并通过一个完整示例演示如何编写自定义组件，最后概览 `contrib/` 下的四个官方组件模块。

## 4.1 Component 接口契约

`Component` 接口定义在 `component.go`，由 `Name` 方法和内嵌的 `LifecycleManaged` 接口组成：

```go
type LifecycleManaged interface {
	Init(app App) error
	Start(ctx context.Context) error
	Stop(ctx context.Context)
}

type Component interface {
	Name() string
	LifecycleManaged
}
```

四个方法的契约如下：

- `Name() string`：组件名称，用于启动/停止日志中的标识。框架不检查唯一性，多个实例可以重名。
- `Init(app App) error`：注册组件时（即 `app.Hooks(lynx.Components(...))` 调用时）**同步**执行，用于初始化依赖——可以通过参数 `app` 访问 `app.Config()`、`app.Logger()`、`app.Context()` 等。返回 error 会让 `Hooks` 直接返回错误，启动失败。
- `Start(ctx context.Context) error`：`cli.Run()` 启动后，每个组件在 run group 中作为独立 actor **并发**调用。通常是阻塞式的（监听端口、消费消息），收到 `ctx` 取消时应返回。任何一个组件的 `Start` 返回（无论是否出错）都会触发整个应用的优雅关闭（见 3.1 节并发模型）。
- `Stop(ctx context.Context)`：关闭阶段由 run group 的中断函数调用，用于释放资源。注意框架是先调用 `Stop` 再取消组件 Context（见 3.1 节），因此 `Stop` 中不要等待 `ctx.Done()`。

注册组件通过 Hooks 完成：

```go
return app.Hooks(lynx.Components(myComponent))
```

每个通过 `lynx.Components` 注册的组件会获得一个独立的 Context（注册时创建），`Start` 和 `Stop` 收到的都是这个 Context。

## 4.2 ComponentBuilder 与多实例

当同一类组件需要运行多个实例时（例如同一个 Kafka 消费组起 3 个 consumer），直接 new 多个组件会很啰嗦，`ComponentBuilder` 为此而生（定义同样在 `component.go`）：

```go
type ComponentBuilder interface {
	Build() Component
	Options() BuildOptions
}

type BuildOptions struct {
	Instances int `json:"instances"` // 实例数
}
```

注册方式与组件类似，使用 `lynx.ComponentBuilders`：

```go
return app.Hooks(lynx.ComponentBuilders(myBuilder))
```

框架对 builder 的处理逻辑（`lynx.go` 的 `addComponentBuilders`）：

1. 调用 `Options()` 获取构建选项，`Instances` 为 0 时按 1 处理；
2. 循环调用 `Instances` 次 `Build()`，每次得到一个**全新**的组件实例；
3. 把这些实例逐一走与 `lynx.Components` 相同的注册流程（各自独立 `Init`/独立 Context/独立 run group actor）。

也就是说，`Instances: 3` 等价于注册三个互不影响的组件实例，`Build()` 必须每次返回新对象，各实例之间不应共享会互相干扰的状态。

框架还预定义了两个辅助类型，供依赖注入场景批量提供 builder：

```go
type ComponentBuilderSet []ComponentBuilder
type ComponentBuilderSetFunc func() ComponentBuilderSet
```

`boot` 包的 Wire 引导流程（`boot.Bind`）会消费 `ComponentBuilderSetFunc`，把返回的 builder 集合一次性注册进应用，用法见 `_examples/boot/provides.go`。

## 4.3 ServerLike 与 CheckHealth 扩展接口

很多组件（服务器、Broker、调度器）还需要对外报告"自己是否健康"。Lynx 复用 gocloud.dev 的 `health.Checker` 接口，定义了 `ServerLike`：

```go
type ServerLike interface {
	health.Checker // CheckHealth() error
	Component
}
```

`health.Checker` 只有一个方法：`CheckHealth() error`——返回 nil 表示健康，返回 error 表示不健康。

组件不需要显式声明自己实现了 `ServerLike`：框架在注册每个组件时会做 `health.Checker` 类型断言（`lynx.go` 的 `addComponents`），只要组件实现了 `CheckHealth() error`，就会被自动收集进应用的健康检查列表。这个列表通过 `app.HealthCheckFunc()` 暴露：

```go
type HealthCheckFunc func() []health.Checker
```

它有两个消费方：

- HTTP 服务器的就绪端点：传入 `http.WithHealthCheck(app.HealthCheckFunc())` 后，`/healthz/readiness` 会依次调用所有收集到的检查器，全部通过才返回 200（见 2.5 节）。
- `app.CLI` 注册的命令：命令执行前会带退避重试地等待所有检查器就绪（`command.go`），保证 CLI 命令不会抢在依赖组件就绪之前运行。

框架内置组件中，`server/grpc` 的 Server、`contrib/pubsub` 的 Broker、`contrib/kafka` 的 Binder、`contrib/schedule` 的 Scheduler 都实现了 `CheckHealth`。典型的实现语义是：未 `Start` 前返回 error，`Start` 成功后返回 nil，`Stop` 后再次返回 error（以 `contrib/schedule` 为例）：

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

## 4.4 自定义组件编写指南

编写自定义组件的要点：

1. 实现 `Name/Init/Start/Stop` 四个方法，`Start` 一般阻塞在 `ctx.Done()` 上；
2. 需要多实例时再配一个实现 `Build/Options` 的 builder，`Build()` 每次返回新实例；
3. 需要参与就绪检查就实现 `CheckHealth() error`，或直接内嵌 `lynx.HealthChecker`；
4. 在 `setup` 回调中用 `lynx.Components` 或 `lynx.ComponentBuilders` 注册。

下面是一个完整可编译的示例：一个 worker 组件内嵌 `HealthChecker` 参与就绪检查，并通过 builder 以 2 个实例运行：

```go
package main

import (
	"context"

	"github.com/lynx-go/lynx"
)

func main() {
	cli := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		return app.Hooks(
			lynx.ComponentBuilders(NewWorkerBuilder("worker", 2)),
		)
	},
		lynx.WithName("custom-component"),
	)
	cli.Run()
}

// worker 是一个自定义组件：实现 Component 接口，并内嵌 HealthChecker，
// 注册后自动成为 /healthz/readiness 的检查项。
type worker struct {
	*lynx.HealthChecker
	name string
}

func (w *worker) Name() string { return w.name }

func (w *worker) Init(app lynx.App) error {
	w.SetHealthy(true) // 也可以等 Start 中就绪后再置为健康
	return nil
}

func (w *worker) Start(ctx context.Context) error {
	<-ctx.Done() // 阻塞直到组件 Context 被取消（即 Stop 返回之后）
	return nil
}

func (w *worker) Stop(ctx context.Context) {
	w.SetHealthy(false)
}

// workerBuilder 负责按指定实例数构建 worker。
type workerBuilder struct {
	name      string
	instances int
}

func NewWorkerBuilder(name string, instances int) lynx.ComponentBuilder {
	return &workerBuilder{name: name, instances: instances}
}

func (b *workerBuilder) Build() lynx.Component {
	// 每次调用都返回新实例，实例之间不共享状态
	return &worker{name: b.name, HealthChecker: &lynx.HealthChecker{}}
}

func (b *workerBuilder) Options() lynx.BuildOptions {
	return lynx.BuildOptions{Instances: b.instances}
}

var _ lynx.ComponentBuilder = (*workerBuilder)(nil)
```

运行后通过日志可以看到两个 worker 实例各自经历了 `initializing component` → `starting component`；按 `Ctrl+C` 后各自收到 `Stop`。由于内嵌了 `HealthChecker`，两个实例都会被收集为就绪检查项。

## 4.5 contrib 模块概览

`contrib/` 下的四个模块是框架官方维护的组件，各自是独立的 Go module，按需引入：

```bash
go get github.com/lynx-go/lynx/contrib/pubsub
go get github.com/lynx-go/lynx/contrib/kafka
go get github.com/lynx-go/lynx/contrib/schedule
go get github.com/lynx-go/lynx/contrib/zap
```

### pubsub：消息发布订阅（Broker/Router/Handler）

`contrib/pubsub` 基于 Watermill 提供进程内/跨进程的事件发布订阅。核心概念：

- `Broker`：事件总线，本身是 `ServerLike` 组件，提供 `Publish`/`Subscribe`。`NewBroker(opts, binders)` 创建时不传 Publisher/Subscriber 则默认使用进程内 GoChannel。
- `Binder`：把外部消息系统（如 Kafka）绑定到 Broker 的桥接层，见下文的 kafka 模块。
- `Router`：把一组 `Handler` 注册到 Broker 的组件。`Handler` 接口由 `EventName()`、`HandlerName()`、`HandlerFunc()` 三个方法组成。

用法（取自 `_examples/pubsub/main.go`）：

```go
broker := pubsub.NewBroker(pubsub.Options{}, []pubsub.Binder{binder})
if err := app.Hooks(lynx.Components(broker)); err != nil {
	return err
}
router := pubsub.NewRouter(broker, []pubsub.Handler{
	&helloHandler{},
})
if err := app.Hooks(lynx.Components(router)); err != nil {
	return err
}
```

Handler 的实现：

```go
type helloHandler struct{}

func (h *helloHandler) EventName() string   { return "hello" }
func (h *helloHandler) HandlerName() string { return "helloHandler" }
func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *message.Message) error {
		log.InfoContext(ctx, "hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)
```

发布事件：

```go
_ = broker.Publish(ctx, "hello",
	pubsub.NewJSONMessage(map[string]any{"message": "hello"}),
	pubsub.WithMessageKey(uuid.NewString()),
)
```

### kafka：Kafka 绑定（Binder）

`contrib/kafka` 提供 `pubsub.Binder` 的 Kafka 实现。通过 `BinderOptions` 声明订阅（`SubscribeOptions`）与发布（`PublishOptions`）配置，`MappedEvent` 把 Kafka topic 映射为 pubsub 事件名（取自 `_examples/pubsub/main.go`）：

```go
binder := kafka.NewBinder(kafka.BinderOptions{
	SubscribeOptions: map[string]kafka.ConsumerOptions{
		"hello": {
			Brokers:     []string{"127.0.0.1:19092"},
			Topic:       "topic_hello",
			Group:       "consumer_hello",
			Instances:   3, // 3 个 consumer 实例，就是 ComponentBuilder 的多实例机制
			MappedEvent: "hello",
		},
	},
	PublishOptions: map[string]kafka.ProducerOptions{
		"hello": {
			Brokers:     []string{"127.0.0.1:19092"},
			Topic:       "topic_hello",
			MappedEvent: "hello",
		},
	},
})
```

注册顺序有一个硬性要求（`_examples/pubsub/main.go` 中的注释）：binder 的 consumer builders 在 `Init()` 中才完成初始化，因此 `binder.ConsumerBuilders()` 必须在 binder 注册**之后**单独注册：

```go
if err := app.Hooks(lynx.Components(broker)); err != nil {
	return err
}
if err := app.Hooks(lynx.Components(binder)); err != nil {
	return err
}
// 因为 binder 中需要先在 Init() 中初始化 consumer builders，所以 binder.ConsumerBuilders() 不能和 binder 同时注入
if err := app.Hooks(lynx.ComponentBuilders(binder.ConsumerBuilders()...)); err != nil {
	return err
}
```

`ConsumerOptions.Instances` 字段会被透传到 builder 的 `BuildOptions.Instances`，上例即为 `hello` 事件启动 3 个并发 consumer。

### schedule：定时任务（Scheduler/Task）

`contrib/schedule` 基于 robfig/cron 提供定时任务调度。`Task` 接口由 `Name()`、`Cron()`（cron 表达式，支持秒级和 `@every 5s` 这类描述符）、`HandlerFunc()` 组成。用法（取自 `_examples/schedule/main.go`）：

```go
scheduler, err := schedule.NewScheduler([]schedule.Task{task1}, schedule.WithLogger(app.Logger()))
if err != nil {
	return err
}
return app.Hooks(lynx.Components(scheduler))
```

Task 的实现：

```go
type task struct{}

func (t *task) Name() string { return "TaskExample" }
func (t *task) Cron() string { return "@every 5s" }
func (t *task) HandlerFunc() schedule.HandlerFunc {
	return func(ctx context.Context) error {
		log.InfoContext(ctx, "task triggered")
		return nil
	}
}

var _ schedule.Task = new(task)
```

`Scheduler` 实现了 `CheckHealth`：任务 handler 中的 panic 会被 recover 并记录日志，不会中断调度器。

### zap：日志集成

`contrib/zap` 把 zap 包装成 `*slog.Logger`，日志级别读取配置中的 `logging.level`（或 `log_level`，默认 `debug`），并自动附加 `service_id`、`service_name`、`version` 三个字段。一行接入（取自 `_examples/pubsub/main.go`）：

```go
app.SetLogger(zap.MustNewLogger(app))
```

如果需要在退出前 flush 缓冲日志，可以改用 `NewSyncableLogger`，在 `OnStop` 钩子中调用其 `Sync()` 方法。

## 4.6 下一步

- [第 5 章：服务器](./05-servers.md) - 学习框架内置 HTTP/gRPC 服务器组件的全部配置项与可观测性接入
