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
	opts := lynx.NewOptions(
		lynx.WithName("http-example"),
		lynx.WithVersion("1.0.0"),
	)

	cli := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		router := http.NewRouter()
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
go run main.go
```

访问 http://localhost:8080 即可看到 JSON 响应：

```json
{"hello":"world","from":"http-example","id":"..."}
```

代码要点：

- `lynx.NewOptions` 通过 `WithName`、`WithVersion` 等选项配置应用元信息。
- `lynx.New(opts, setup)` 创建应用实例，`setup` 回调中通过 `app.Hooks(lynx.Components(...))` 注册组件。
- `http.NewServer` 创建一个 HTTP 服务器组件，`WithAddr` 指定监听地址，`WithHealthCheck` 开启健康检查（见 2.5 节）。
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
		f.StringP("config", "c", "./configs", "config file path")
		f.String("addr", "", "http listen address")
		f.StringP("log_level", "l", "debug", "log level")
	}),
	lynx.WithBindConfigFunc(func(f *pflag.FlagSet, v *viper.Viper) error {
		if c, _ := f.GetString("config"); c != "" {
			v.SetConfigFile(c)
		}
		v.SetEnvPrefix("LYNX_")
		v.AutomaticEnv()

		if err := v.BindEnv("addr", "LYNX_ADDR"); err != nil {
			return err
		}
		return nil
	}),
)
```

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

配置来源的优先级遵循 Viper 的规则：命令行参数、环境变量、配置文件可以组合使用。上面的示例同时演示了三种来源——`--addr` 命令行参数、`LYNX_ADDR` 环境变量和 `config.yaml` 文件。

如果只需要框架内置的配置参数（`--config`、`--config-type`、`--config-dir`、`--log-level`），可以直接使用 `lynx.WithUseDefaultConfigFlagsFunc()`，无需自定义上述两个函数。

## 2.4 CLI 模式

Lynx 也可以用来构建执行一次性命令的 CLI 工具。通过 `app.CLI(cmd)` 注册命令函数，应用启动后执行该命令，执行完毕后自动退出。下面是一个最小示例（改编自 `_examples/cli/main.go`）：

```go
package main

import (
	"context"
	"fmt"

	"github.com/lynx-go/lynx"
)

func main() {
	opts := lynx.NewOptions(
		lynx.WithName("cli-example"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)

	cli := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		return app.CLI(func(ctx context.Context) error {
			fmt.Println("hello cli")
			return nil
		})
	})
	cli.Run()
}
```

运行：

```bash
go run main.go
```

输出 `hello cli` 后进程自动退出。`app.CLI` 注册的命令同样运行在 Lynx 的生命周期管理中，可以与 `OnStart`/`OnStop` 钩子及其他组件（如 PubSub Broker）配合使用，完整示例见 `_examples/cli/main.go`。

## 2.5 健康检查端点

为 HTTP 服务器传入 `http.WithHealthCheck(app.HealthCheckFunc())` 后，服务器会自动暴露两个健康检查端点（由底层 gocloud.dev 服务器提供）：

- `/healthz/liveness`：存活检查，进程存活即返回 200，用于探活。
- `/healthz/readiness`：就绪检查，依次调用所有注册的健康检查器，全部通过才返回 200，否则返回 503。

验证方式：

```bash
curl -i http://localhost:8080/healthz/liveness
curl -i http://localhost:8080/healthz/readiness
```

`app.HealthCheckFunc()` 会收集所有实现了 `health.Checker` 接口的组件作为就绪检查项。框架还提供了开箱即用的 `lynx.HealthChecker`，可通过 `SetHealthy(true/false)` 动态控制就绪状态：

```go
checker := &lynx.HealthChecker{}
checker.SetHealthy(true)
```

这对于需要"预热后再接流量"或"运维时临时摘流"的场景非常实用。

## 2.6 下一步

- [第 3 章：核心概念](./03-core-concepts.md) - 了解生命周期、Hooks、Options 与配置管理等核心设计理念
