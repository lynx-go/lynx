# telemetry

```go
import "github.com/lynx-go/lynx/contrib/telemetry"
```

以服务形式托管 OpenTelemetry 的生命周期：创建 TracerProvider 与 MeterProvider、设置为 otel 全局值，并在应用停止时自动 flush 与关闭。

## 能力要点

- `New(opts ...Option) lynx.Service`（telemetry.go:97）：创建托管 OTel 生命周期的服务，服务名 `otel`（telemetry.go:118）。
- `Init` 创建 provider 并设置为 otel 全局值（`otel.SetTracerProvider` / `otel.SetMeterProvider` / `otel.SetTextMapPropagator`，telemetry.go:145-147）——有意的全局副作用；重复 Init 返回错误（telemetry.go:127-129）。
- `Start` 阻塞至应用关闭（actor 语义，telemetry.go:153）；`Stop` 自动 flush 并 shutdown，错误经 `lynx.ShutdownErrors` 聚合返回（telemetry.go:166-178）。
- 默认导出（newProviders，telemetry.go:189-219）：noop trace exporter（span 直接丢弃，生产忘配 exporter 不会向 stdout 倒 trace）+ Prometheus metric reader + W3C TraceContext/Baggage propagator。Prometheus 指标需自行挂载 `/metrics`（如 `promhttp.Handler()`）。
- Go runtime 指标（goroutine/GC/内存）默认注册到 MeterProvider，随指标管线一并输出，零配置获得进程级可观测基线；`WithoutRuntimeMetrics()` 关闭。容器环境的 CPU 配额感知（GOMAXPROCS 修正）由 Go 1.25+ runtime 内建，无需 automaxprocs。
- 接入即启用总线 trace 传播：Init 设置的全局 propagator（TraceContext+Baggage）被 `eventbus` 发布/消费路径使用——跨进程 Bus（Watermill/Kafka）的 trace 自动续链（发布注入 `traceparent`、消费开 `consume <topic>` span），见 [design-eventbus §5.7](../../docs/design-eventbus.md)。
- `Init(ctx)` 在 ctx 非 nil 且未显式 `WithResource` 时，自动以应用名（`lynx.Meta(ctx.Context()).Name`）构建 `service.name` 资源属性（telemetry.go:131-137）。

选项（telemetry.go:41-90）：

| 选项 | 说明 |
| --- | --- |
| `WithTraceExporter(exporter sdktrace.SpanExporter)` | 自定义 trace exporter，默认 noop |
| `WithStdoutTrace()` | stdout pretty print 输出 span，供开发调试；仅在未设置 `WithTraceExporter` 时生效 |
| `WithMetricReader(reader sdkmetric.Reader)` | 自定义 metric reader（如 OTLP exporter），默认 Prometheus |
| `WithPropagator(p propagation.TextMapPropagator)` | 自定义 propagator，默认 TraceContext + Baggage 组合 |
| `WithResource(r *resource.Resource)` | OTel Resource（如 service.name），nil 时 SDK 默认并自动附加应用名 |
| `WithoutRuntimeMetrics()` | 关闭 Go runtime 指标注册（goroutine/GC/内存，缺省开启） |

## 快速开始

（取自 `_examples/http/main.go:36`）

```go
package main

import (
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/telemetry"
	"go.opentelemetry.io/otel"
)

func main() {
	lynx.NewRunner(func(app lynx.App) error {
		app.Register(telemetry.New())
		// 业务指标必须在服务注册之后创建（Init 同步执行），
		// 否则拿到的是 noop meter（见 _examples/http/metrics.go）：
		counter, err := otel.Meter("demo").Int64Counter("demo.requests.total")
		if err != nil {
			return err
		}
		counter.Add(app.Context(), 1)
		return nil
	}, lynx.WithName("otel-demo")).Run()
}
```

生产环境接 OTLP collector（完整 exporter 构建见 `docs/05-servers.md` 5.4.3 节）：

```go
app.Register(telemetry.New(
	telemetry.WithTraceExporter(otlpTraceExporter), // 替换默认 noop
	telemetry.WithMetricReader(otlpMetricReader),   // 替换默认 Prometheus
	telemetry.WithStdoutTrace(),                    // 本地调试：span 打到 stdout
))
```

## 与 lynx 核心的集成

- `app.Register(telemetry.New())`：服务实现 `lynx.Service`，Init 同步创建并设置全局 provider；此后 `server/http`、`server/grpc` 的 provider 参数为 nil 时直接使用全局值，无需逐项传入。
- Stop 错误随框架 `Run()` 统一上抛；Stop 后 otel 全局 provider 不复位（单进程单次生命周期场景无碍，取舍见 telemetry.go:160-165 注释）。
- 默认 Prometheus reader 使用 otel 全局注册表且 Stop 不 unregister，请按单实例使用（telemetry.go:183-188 注释）；需重建的场景用 `WithMetricReader` 传入自管注册表的 reader。

## 相关文档与示例

- `docs/05-servers.md` 5.4.1「contrib/telemetry（框架托管）」、5.4.3「OTLP Exporter」、5.4.4「Prometheus 指标与 /metrics」
- `docs/04-service-system.md` 4.5 节「telemetry：可观测性托管」
- `_examples/http/`：`main.go:36` 注册服务、`metrics.go` 创建业务指标、`main.go:72` 挂载 `/metrics`
