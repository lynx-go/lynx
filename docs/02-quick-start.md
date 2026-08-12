# 2. 快速开始

本章带你从零开始创建一个 Lynx 应用：安装框架、运行第一个 HTTP 服务、接入配置文件、使用 CLI 模式，并了解内置的健康检查端点。

## 2.1 安装

Lynx 要求 Go 1.25 及以上版本。在项目中通过 `go get` 安装：

```bash
go get github.com/lynx-go/lynx
```

## 2.2 第一个 HTTP 服务

下面是一个最小可运行的 HTTP 服务示例（改编自 `_examples/http/main.go`，去掉了可观测性等进阶配置）。创建 `main.go`：

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
		router := gohttp.NewServeMux()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			meta := lynx.Meta(app.Context())
			out, _ := json.Marshal(map[string]any{
				"hello": "world",
				"from":  meta.Name,
				"id":    meta.ID,
			})
			_, _ = rw.Write(out)
		})

		app.Register(http.NewServer(router,
			http.WithAddr(":8080"),
			http.WithHealthCheckers(app.HealthCheckers),
		))
		return nil
	},
		lynx.WithName("http-example"),
		lynx.WithVersion("1.0.0"),
	)
	cli.Run()
}
```

运行：

```bash
go run main.go
```

访问 http://localhost:8080 即可看到 JSON 响应：

```json
{"hello":"world","from":"http-example","id":"..."}
```

代码要点：

- `lynx.NewOptions` 通过 `WithName`、`WithVersion` 等选项配置应用元信息。
- `lynx.NewRunner(setup, opts...)` 创建应用实例，`setup` 回调中通过 `app.Register(...)` 注册服务。
- `http.NewServer` 创建一个 HTTP 服务器服务，`WithAddr` 指定监听地址，`WithHealthCheckers` 开启健康检查（见 2.5 节）。
- `cli.Run()` 启动应用并阻塞，直到收到退出信号后优雅关闭。

## 2.3 使用配置文件

Lynx 基于 Viper 提供配置管理。通过 `WithSetFlagsFunc` 定义命令行参数，通过 `WithBindConfigFunc` 把参数绑定到配置来源。创建 `config.yaml`：

```yaml
addr: ":8080"
log_level: "debug"
```

代码中声明参数并绑定配置文件（以下代码取自 `_examples/http/main.go`）：

```go
opts := lynx.NewOptions(
	lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
		f.StringP("config", "c", "./config.yaml", "config file path")
		f.String("addr", "", "http listen address")
		f.StringP("log_level", "l", "debug", "log level")
	}),
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
	}),
)
```

> 注意：上例通过 `WithSetFlagsFunc` 完全自定义了命令行参数（如 `log_level`/`-l`）。
> 若不覆盖，框架默认启用内置 flags（`-c/--config`、`--config-type`、`--config-dir`、
> `--log-level`，见 `DefaultSetFlagsFunc`），两者键名不同，混用时以实际注册的为准。

在 `setup` 回调中读取配置：

```go
config := &Config{}
if err := app.Config().Unmarshal(config); err != nil {
	return err
}
```

也可以直接按键读取单个配置项：

```go
addr := app.Config().GetString("addr")
```

`app.Config()` 返回 `lynx.Config` 接口（默认实现适配 `*viper.Viper`），`Unmarshal` 按结构体 tag（`mapstructure` 或 `json`）解码，`GetString` 等类型化方法按点分路径取单值（详见第 3 章 3.4 节）。

配置来源的优先级遵循 Viper 的规则：命令行参数、环境变量、配置文件可以组合使用。上面的示例同时演示了三种来源——`--addr` 命令行参数、`LYNX_ADDR` 环境变量和 `config.yaml` 文件。

如果只需要框架内置的配置参数（`--config`、`--config-type`、`--config-dir`、`--log-level`），无需任何配置：默认启用框架内置的参数声明与绑定。不需要命令行参数时可显式关闭：`lynx.WithDisableConfigFlags()`。

## 2.4 CLI 模式

Lynx 也可以用来构建执行一次性命令的 CLI 工具。通过 `app.Command(cmd)` 注册命令函数，应用启动后执行该命令，执行完毕后自动退出。下面是一个最小示例（改编自 `_examples/cli/main.go`）：

```go
package main

import (
	"context"
	"fmt"

	"github.com/lynx-go/lynx"
)

func main() {
	cli := lynx.NewRunner(func(app lynx.App) error {
		return app.Command(func(ctx context.Context) error {
			fmt.Println("hello cli")
			return nil
		})
	},
		lynx.WithName("cli-example"),
	)
	cli.Run()
}
```

运行：

```bash
go run main.go
```

输出 `hello cli` 后进程自动退出。`app.Command` 注册的命令同样运行在 Lynx 的生命周期管理中，可以与 `OnStart`/`OnStop` 钩子及其他服务（如 PubSub Broker）配合使用，完整示例见 `_examples/cli/main.go`。

## 2.5 健康检查端点

为 HTTP 服务器传入 `http.WithHealthCheckers(app.HealthCheckers)` 后，服务器会自动暴露两个健康检查端点：

- `/healthz/liveness`：存活检查，进程存活即返回 200，用于探活。**不消费**检查器聚合——配置关停排水（`WithDrainTimeout`，见 3.7 节）时，排水期间 liveness 仍返回 200。
- `/healthz/readiness`：就绪检查，依次调用所有注册的健康检查器，全部通过才返回 200，任一失败返回 503 + 错误正文。配置 `WithDrainTimeout` 后，排水期间框架内部的 `drainChecker` 使该端点返回 503（LB 摘流）。

验证方式：

```bash
curl -i http://localhost:8080/healthz/liveness
curl -i http://localhost:8080/healthz/readiness
```

`app.HealthCheckers` 会收集所有实现了 `lynx.Checker` 接口的服务作为就绪检查项——收集发生在服务注册时，框架对每个通过 `app.Register` 注册的服务做 `Checker` 类型断言，通过断言的才会加入就绪检查列表。

框架还提供了开箱即用的 `lynx.HealthChecker`，可通过 `SetHealthy(true/false)` 动态控制就绪状态。需要注意：`lynx.HealthChecker` 只实现了 `Checker` 接口，并不是 `Service`，单独创建它不会产生任何效果。正确的用法是把它内嵌到自己的服务中，再把服务注册进应用：

```go
type myService struct {
	*lynx.HealthChecker
}

func (c *myService) Name() string             { return "my-service" }
func (c *myService) Init(ctx lynx.AppContext) error { c.SetHealthy(true); return nil }
func (c *myService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (c *myService) Stop(ctx context.Context) error { return nil }
```

注册服务（注意初始化内嵌的 `HealthChecker`，否则为空指针）：

```go
app.Register(&myService{HealthChecker: &lynx.HealthChecker{}})
return nil
```

由于内嵌，`myService` 自动满足 `Checker` 接口，注册后即成为 `/healthz/readiness` 的检查项；之后在业务逻辑中调用 `c.SetHealthy(false)` 即可让就绪检查返回 503。这对于需要"预热后再接流量"或"运维时临时摘流"的场景非常实用。

## 2.6 下一步

- [第 3 章：核心概念](./03-core-concepts.md) - 了解生命周期、Hooks、Options 与配置管理等核心设计理念
