# 3. 核心概念

本章介绍 Lynx 的核心设计理念：应用生命周期与并发模型、Hooks 机制、Options 选项、配置管理、Context 辅助函数以及优雅关闭。这些概念贯穿整个框架，理解它们之后再去读第 4 章的服务系统会顺畅得多。

## 3.1 应用生命周期

一个 Lynx 应用的完整生命周期由 `lynx.NewRunner` 和 `cli.Run()` 串起来：

1. `lynx.NewRunner(setup, opts...)` 创建应用实例（返回 `*Runner`）：先调用 `EnsureDefaults` 补全 Options，再解析命令行参数、读取配置文件，最后把应用名称、ID、版本注入应用 Context（见 3.5 节）。
2. `cli.Run()` 首先调用 `setup` 回调：在这里注册服务和钩子。服务的 `Init(ctx AppContext)` 在注册时（即 `app.Register` 调用时）同步执行，返回 error 会被记录为首个注册错误，由 `Run()` 统一返回导致启动失败。
3. `setup` 返回后进入 `Run()`：启动所有服务的 `Start(ctx)`，并阻塞等待退出信号。
4. 收到退出信号（或某个执行单元结束）后进入关闭流程，依次执行 `OnStop` 钩子并调用各服务的 `Stop(ctx)`。

即每个服务遵循 `Init → Start → Stop` 的调用顺序：

- `Init(ctx AppContext)`：注册服务时同步调用，用于初始化依赖。参数是 `lynx.AppContext`（`Context`/`Config`/`Logger`/`HealthCheckers`/`Close`），服务不依赖完整的 `App` 接口（见 3.6 节 AppContext 接口说明）。
- `Start`：`Run()` 启动后并发调用，通常是阻塞式的（如监听端口、消费消息），其 `ctx` 被取消时应返回。
- `Stop(ctx) error`：关闭阶段调用，用于释放资源；返回的错误由框架收集，与 OnStop 钩子错误一起随 `Run()` 上抛。

### 并发模型：run group

Lynx 使用 oklog/run 的 `run.Group` 管理所有并发执行单元。每个通过 `app.Register` / `app.RegisterFactories` 注册的服务是一个 actor；此外 `Run()` 还会注册一个信号 actor。

- `OnStart` 钩子不占用 run group actor：在 `Run()` 中、服务启动前按注册顺序串行执行，全部成功后才启动服务（见 3.2 节）。
- 信号 actor：监听退出信号（见 3.6 节）或应用 Context 取消。一旦触发，先在 actor 内按顺序执行所有 `OnStop` 钩子，然后返回，run group 随之中断各服务。

run group 的语义是：所有服务 actor 并发运行；一旦有任何一个 actor 返回——服务 `Start` 出错、CLI 命令执行完毕（`app.Command` 注册的命令结束时调用 `app.Close()`）、或信号 actor 返回——框架会中断其余所有 actor，整个应用随之进入统一关闭流程。这意味着任何一个服务失败都会触发整体优雅关闭，不会出现"半个应用还在跑"的状态。

需要注意一个细节：每个服务拥有独立的 Context（注册服务时创建）。关闭时 run group 对每个服务 actor 先调用 `Stop(ctx)`，再取消其 Context。因此服务的 `Stop` 实现不要等待 `ctx.Done()`——它永远不会等到；`Start` 中阻塞在 `<-ctx.Done()` 上的逻辑会在 `Stop` 返回后被解除。

## 3.2 Hooks 与错误聚合

钩子函数的类型是 `HookFunc`：

```go
type HookFunc func(ctx context.Context) error
```

通过 `app.OnStart` / `app.OnStop` 直接注册：

```go
app.OnStart(func(ctx context.Context) error {
	// 启动时执行，例如预热缓存、释放 setup 阶段的临时资源
	return nil
})
app.OnStop(func(ctx context.Context) error {
	// 关闭时执行，例如 flush 数据、注销服务发现
	return nil
})
return nil
```

两类钩子的执行时机和语义不同：

- `OnStart`：在 `Run()` 启动阶段、服务启动前按注册顺序串行执行，收到的 `ctx` 是应用 Context。任何一个钩子返回 error，`Run()` 立即返回该错误，服务不会启动。
- `OnStop`：在关闭阶段、服务 `Stop` 之前按注册顺序串行执行，收到带有 `ShutdownTimeout` 超时的 `ctx`（见 3.6 节）；单个钩子阻塞超过时限会被判定超时并继续执行后续钩子，不会挂起整个关闭流程。

`OnStop` 钩子的错误处理使用 `errors.go` 中的 `ShutdownErrors` 做聚合：某个钩子出错不会中断后续钩子的执行，所有错误被收集后以分号连接成一条日志输出。`ShutdownErrors` 的 API：

- `Add(err)`：追加错误，nil 会被忽略。
- `HasErrors()`：是否收集到错误。
- `Error()`：返回所有错误消息以 `"; "` 连接的字符串。
- `Errors()`：返回收集到的错误切片副本。

该类型内部使用互斥锁保护，可并发使用。框架自身只在关闭流程中用到它：聚合结果只记录日志，不会向上传递——进程此时已经在退出路径上。

`_examples/boot/main.go` 中有 `OnStop` 的实际用例：Wire 构建的依赖图返回了 `cleanup` 函数，示例把它放在 `OnStop` 钩子里执行，在应用优雅关闭时释放资源。

## 3.3 Options

`lynx.NewOptions` 通过函数式选项创建 `Options`，全部可用选项如下：

| 选项 | 作用 |
| --- | --- |
| `WithID(id)` | 实例 ID，默认为主机名 |
| `WithName(name)` | 应用名称，默认 `"lynx-app"` |
| `WithVersion(v)` | 应用版本 |
| `WithSetFlagsFunc(f)` | 自定义命令行参数声明（见 3.4 节） |
| `WithBindConfigFunc(f)` | 自定义配置绑定逻辑（见 3.4 节） |
| `WithDisableConfigFlags()` | 关闭默认的命令行参数声明与绑定（默认开启） |
| `WithExitSignals(signals...)` | 自定义触发优雅关闭的信号列表 |
| `WithShutdownTimeout(d)` | OnStop 钩子关闭超时，默认 5 秒 |
| `WithStopTimeout(d)` | 单个服务 Stop 最长等待时长，默认 5 秒 |

`NewOptions` 自身已经填充了部分默认值：`ID` 取 `os.Hostname()`，`Name` 为 `DefaultName`，`ShutdownTimeout` 为 5 秒，`StopTimeout` 为 5 秒，`ExitSignals` 为默认信号列表，并默认启用内置配置 flags（`SetFlagsFunc`/`BindConfigFunc` 默认取 `DefaultSetFlagsFunc`/`DefaultBindConfigFunc`）。

### 校验规则

`Options.Validate()` 定义了两条校验规则（相关常量与错误均定义在 `options.go`）：

- 名称长度不能超过 63 个字符，否则返回 `ErrNameTooLong`。
- `ShutdownTimeout` 大于 0 时，必须在 `[MinTimeout, MaxTimeout]` 区间内（该区间为 ShutdownTimeout 与 StopTimeout 共用），即不小于 1 秒（否则 `ErrShutdownTimeoutTooSmall`）、不大于 5 分钟（否则 `ErrShutdownTimeoutTooLarge`）。`ShutdownTimeout` 为 0 视为合法，表示"使用默认值"。`StopTimeout` 的校验区间相同（`ErrStopTimeoutTooSmall`/`ErrStopTimeoutTooLarge`）。

`lynx.NewRunner` 在调用 `EnsureDefaults()` 补齐默认值后会自动调用 `Validate()`，校验失败会让 `Run()`/`RunE()` 返回对应错误。如果需要在创建应用之前单独校验配置（例如来自外部输入），也可以显式调用：

```go
opts := lynx.NewOptions(lynx.WithName(nameFromInput))
if err := opts.Validate(); err != nil {
	log.Fatal(err)
}
```

### EnsureDefaults

`EnsureDefaults()` 在 `lynx.NewRunner` 内部自动调用，为未设置的字段填充默认值：

- `ID` 为空时取主机名；
- `Name` 为空时取 `DefaultName`（`"lynx-app"`）；
- `ShutdownTimeout` 为 0 时取 `DefaultShutdownTimeout`（5 秒）；
- `ExitSignals` 为空时使用默认信号列表（见 3.6 节）。

## 3.4 配置管理

Lynx 的配置体系基于 Viper（读取与合并）加 pflag（命令行参数），在应用初始化阶段按固定顺序执行：

1. 调用 `SetFlagsFunc` 声明参数，并解析 `os.Args[1:]`；
2. 调用 `BindConfigFunc` 把参数绑定到应用配置（配置文件路径、环境变量等）；
3. 调用 `ReadInConfig` 读取配置文件；
4. 调用 `BindPFlags` 把命令行参数合并进配置。

之后在 `setup` 回调中通过 `app.Config()` 获取 `lynx.Config` 接口读取配置（默认由 `*viper.Viper` 适配实现）——可以 `Unmarshal` 到结构体，也可以按 `GetString` 等方法逐键读取（用法见第 2 章 2.3 节）。`BindConfigFunc` 接收的是 `lynx.ConfigSource`（`Config` 的超集），绑定阶段所需的 `SetFile`/`AutomaticEnv`/`BindEnv` 等方法都包含在接口内；需要更完整的 Viper API 时，可自行创建 `*viper.Viper` 并用 `lynx.NewViperConfig` 包装。

### 内置参数

默认启用框架内置的四个参数（`DefaultSetFlagsFunc`）及其绑定逻辑（`DefaultBindConfigFunc`），无需任何选项；不需要命令行参数时可显式关闭（`WithDisableConfigFlags`）：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--config`, `-c` | 空 | 配置文件完整路径 |
| `--config-type` | `yaml` | 配置文件类型 |
| `--config-dir` | 空 | 配置文件搜索目录 |
| `--log-level` | 空（缺省 `info`） | 日志级别；未显式传入时回退配置键 `logging.level` → `log-level` → `log_level` |

未知命令行参数（如 `go test` 二进制的 `-test.*`）会被忽略，不阻断启动。

### 环境变量

环境变量支持不是框架自动开启的，需要在自定义的 `WithBindConfigFunc` 中显式启用（取自 `_examples/http/main.go`）：

```go
lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
	if cf, _ := f.GetString("config"); cf != "" {
		c.SetFile(cf)
	}
	c.SetEnvPrefix("LYNX_")
	c.AutomaticEnv()

	if err := c.BindEnv("addr", "LYNX_ADDR"); err != nil {
		return err
	}
	return nil
})
```

`SetEnvPrefix` 加 `AutomaticEnv` 让 Viper 自动读取带前缀的环境变量；`BindEnv` 则把指定配置键精确绑定到某个环境变量。配置来源的优先级遵循 Viper 的规则：命令行参数、环境变量、配置文件可以组合使用。

另外，应用名称、ID、版本这三个元信息也参与配置合并：配置中的 `service.name`、`service.id`、`service.version` 键优先，覆盖 `Options` 中的对应值；旧顶层键 `name`、`id`、`version` 的回退已于 v1.0 移除，不再参与合并。最终注入应用 Context 的是合并后的结果。

### 配置接口

框架对配置的访问抽象为两个通用接口，与具体配置库解耦（默认实现适配 `*viper.Viper`，通过 `lynx.NewViperConfig` 包装）：

- `lynx.Config`：**只读**配置接口，`app.Config()` 返回。`Get(path)` 按点分路径取值（如 `"logging.level"`），`GetString`/`GetBool`/`GetInt`/`GetStringMap`/`GetStringSlice` 是类型化取值，`IsSet` 判断键是否存在，`Unmarshal(out)` 把配置整体解码到结构体。
- `lynx.ConfigSource`：`Config` 的超集，供初始化绑定阶段（`BindConfigFunc`）使用，额外提供 `Set` 与配置源管理方法：`SetFile`（配置文件路径）、`AddSearchPath`（搜索目录）、`SetFileFormat`（文件格式）、`SetEnvPrefix`（环境变量前缀）、`AutomaticEnv`（环境变量自动匹配）、`BindEnv`（显式环境变量绑定）。

接入其他配置库（如 koanf）时，只需实现 `Config` 与 `ConfigSource` 两个接口，并在 `BindConfigFunc` 中完成来源绑定，框架其余部分无需改动。

## 3.5 Context 辅助函数

框架在初始化时把应用元信息注入应用 Context（`app.Context()`），并提供三个取值函数（均定义在 `lynx.go`）：

- `lynx.IDFromContext(ctx)`：实例 ID
- `lynx.NameFromContext(ctx)`：应用名称
- `lynx.VersionFromContext(ctx)`：应用版本

三个函数在值不存在或类型不符时返回空字符串。典型用法是在请求处理或日志中读取应用身份（取自 `_examples/http/main.go`）：

```go
router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
	name := lynx.NameFromContext(app.Context())
	id := lynx.IDFromContext(app.Context())
	out, _ := json.Marshal(map[string]any{
		"hello": "world",
		"from":  name,
		"id":    id,
	})
	_, _ = rw.Write(out)
})
```

## 3.6 AppContext 接口与服务接缝

服务的 `Init` 接收的不是完整的 `App` 接口，而是更窄的 `lynx.AppContext`：

```go
type AppContext interface {
	Context() context.Context
	Config() Config
	Logger(kwargs ...any) *slog.Logger
	HealthCheckers() []Checker
	Close()
}
```

`App` 是 `AppContext` 的超集（`App` 内嵌 `AppContext`，额外提供 `Register`/`OnStart`/`OnStop`/`Command`/`Run`/`SetLogger`）。服务在 `Init` 中只依赖 `AppContext` 的五个方法：读取配置、取日志、访问应用元信息（经 Context）、获取健康检查快照、或请求关闭应用（如一次性命令执行完毕）。测试时只需实现这五个方法，无需为 `App` 的其余方法写空实现。

框架的职责边界：服务不能通过 `AppContext` 注册其他服务或修改生命周期钩子——`Init` 阶段（注册时同步执行）只允许"读取环境、准备资源"。

## 3.7 优雅关闭

### 信号处理

框架默认监听 `SIGTERM`、`SIGQUIT`、`SIGINT` 三个信号（`SIGKILL` 无法被进程捕获，因此不在默认列表中）。可通过 `WithExitSignals` 自定义：

```go
opts := lynx.NewOptions(
	lynx.WithExitSignals(syscall.SIGTERM, syscall.SIGINT),
)
```

除了信号，以下事件同样会进入关闭流程：任一服务 `Start` 返回（正常结束或出错）、`app.Command` 注册的命令执行完毕、用户代码主动调用 `app.Close()`（取消应用 Context）。

### 关闭流程

关闭按固定步骤执行：

0. **排水窗口（可选）**：配置 `WithDrainTimeout` 后，关停信号到达先置位框架内部的 `drainChecker`，使 readiness 聚合（`app.HealthCheckers()`）立即失败——负载均衡器开始摘流；随后等待 `DrainTimeout` 窗口结束，服务在此期间保持运行供在途请求收尾；
1. 取消应用 Context，通知所有监听它的逻辑（包括 OnStart 钩子 actor）退出；
2. 以 `ShutdownTimeout` 为超时创建新 Context，按注册顺序串行执行所有 `OnStop` 钩子，错误通过 `ShutdownErrors` 聚合；
3. run group 中断所有服务 actor：对每个服务先调用 `Stop(ctx)`，再取消其 Context（使 `Start` 中的 `<-ctx.Done()` 解除阻塞）。服务 `Stop` 返回的错误与超时错误同样聚合进 `ShutdownErrors`。

文字时序图：

```
关停信号 / 中断 / app.Close()
  │
  ├─ 置位 drainChecker → readiness 立即失败（LB 摘流）
  ├─ [DrainTimeout] 排水窗口（0 = 跳过，行为与 v1.0 一致）
  ├─ 取消应用 Context
  ├─ [ShutdownTimeout] 串行执行 OnStop 钩子
  └─ 服务 Stop（单个最长 [StopTimeout]，挂死跳过）
```

`ShutdownTimeout` 默认 5 秒（`DefaultShutdownTimeout`），可通过 `WithShutdownTimeout` 调整，合法区间为 1 秒到 5 分钟（见 3.3 节校验规则）。它约束的是 `OnStop` 钩子的总执行时间。服务 `Stop` 的单个最长等待由 `Options.StopTimeout`（默认 5 秒）约束：挂死（如等待 `ctx.Done()`）的 `Stop` 超时后跳过并记录错误，不会阻塞整个关停流程。

`DrainTimeout`（`WithDrainTimeout` 设置）是**独立的第二段预算**，默认 0 表示不启用排水（关停行为与 v1.0 完全一致），取值任意 ≥0 无下限约束。所有关停入口（信号、服务中断、`app.Close()`）统一生效。启用后**总关停时长上界 = `DrainTimeout` + `ShutdownTimeout` + 各服务 `StopTimeout` 叠加的既有上界**；例如 `DrainTimeout=30s` + 默认值，一次完整关停最长约 40 秒。K8s 场景下 `terminationGracePeriodSeconds` 需覆盖该上界，否则进程会在排水窗口内被 SIGKILL，服务来不及优雅停止。

排水只影响 **readiness**（HTTP `/healthz/readiness` 与 gRPC health 探测）：HTTP 的 `/healthz/liveness` 恒返回 200、不消费检查器聚合，排水期间存活探针不受影响（见 5.1 节）。

`Run()` 返回时会把三类错误聚合上抛（`errors.Join`）：run group 的首个 actor 错误、OnStop 钩子错误（含超时）、服务 Stop 错误（含超时）——调用方（如 K8s）可以感知关停失败。

## 3.8 综合示例

下面这个完整示例把本章的概念串起来：自定义 `ShutdownTimeout`、注册 `OnStart`/`OnStop` 钩子、注册一个服务、通过 Context 辅助函数读取应用元信息。运行后按 `Ctrl+C` 可以观察完整的优雅关闭过程。

```go
package main

import (
	"context"
	"time"

	"github.com/lynx-go/lynx"
)

func main() {
	cli := lynx.NewRunner(func(app lynx.App) error {
		app.OnStart(func(ctx context.Context) error {
			app.Logger().Info("on-start hook",
				"name", lynx.NameFromContext(app.Context()),
				"id", lynx.IDFromContext(app.Context()),
				"version", lynx.VersionFromContext(app.Context()),
			)
			return nil
		})
		app.OnStop(func(ctx context.Context) error {
			app.Logger().Info("on-stop hook")
			return nil
		})
		app.Register(&myService{})
		return nil
	},
		lynx.WithName("core-concepts"),
		lynx.WithVersion("1.0.0"),
		lynx.WithShutdownTimeout(10*time.Second),
	)
	cli.Run()
}

type myService struct{}

func (c *myService) Name() string { return "my-service" }

func (c *myService) Init(ctx lynx.AppContext) error { return nil }

func (c *myService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (c *myService) Stop(ctx context.Context) error { return nil }
```

## 3.9 下一步

- [第 4 章：服务系统](./04-service-system.md) - 深入理解 `Service` 接口契约、`ServiceFactory` 与自定义服务编写
