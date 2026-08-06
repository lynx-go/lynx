# 5. 服务器与可观测性

Lynx 在 `server/http` 与 `server/grpc` 两个包中提供了开箱即用的服务器组件，它们都实现了第 4 章介绍的 `Component` 接口，可以直接通过 `app.Register` 注册进应用。本章先逐一介绍两个服务器的全部配置项，再讲解可观测性接入：OpenTelemetry trace/metrics 的开箱即用（`contrib/telemetry`）与手动接入、Prometheus 指标暴露、HTTP 中间件链序，以及日志与链路的关联（`lynx.NewTraceHandler`）。

## 5.1 HTTP 服务器

### 创建服务器

HTTP 服务器定义在 `server/http/server.go`，通过 `NewServer` 创建：

```go
func NewServer(handler http.Handler, opts ...Option) *Server
```

第一个参数是标准的 `http.Handler`，用标准库的 `http.NewServeMux()`（或任意第三方的 mux）即可——Lynx 不绑定任何第三方路由，想换 chi、gin 的 mux 只需把对应的 `http.Handler` 传进来即可。

默认值（`server/http/server.go` 顶部的常量）：

- 监听地址 `:8080`（`DefaultHTTPAddr`）
- 请求读写超时 60 秒（`DefaultTimeout`）
- 优雅关闭超时 10 秒（`DefaultShutdownTimeout`）
- 日志器 `slog.Default()`

返回的 `*Server` 实现了 `lynx.Component`，`Name()` 为 `"http"`，注册方式与其他组件一致。

### Options 一览

全部 Options 定义在 `server/http/server.go` 与 `server/http/middleware.go`：

- `WithAddr(addr string)`：监听地址，默认 `:8080`。
- `WithTimeout(timeout time.Duration)`：请求读写超时，默认 60 秒。该值会同时设置为底层 `http.Server` 的 `ReadHeaderTimeout`、`ReadTimeout` 和 `WriteTimeout`；传入 0 或负数则不设置（保持底层默认值）。
- `WithShutdownTimeout(timeout time.Duration)`：优雅关闭超时，默认 10 秒。调用方 Context 无 deadline 时生效：`Stop` 以它为上限等待 `Shutdown` 排空连接，超时后强制 `Close()` 活动连接，避免长轮询/流式 handler 让关闭无限挂起。
- `WithHealthCheckers(hc lynx.HealthCheckersFunc)`：健康检查器取值函数。传入后服务器自动暴露两个端点：`/healthz/liveness` 恒返回 200，`/healthz/readiness` 依次调用所有收集到的检查器，任一失败返回 503 + 错误正文。通常直接传方法值 `app.HealthCheckers`，收集规则见 2.5 节与 4.3 节。两个端点始终注册；不传该 Option 只是就绪检查列表为空，此时 `/healthz/readiness` 恒返回 200。
- `WithLogger(l *slog.Logger)`：请求日志使用的日志器，默认 `slog.Default()`。
- `WithRequestLog(requestLog bool)`：是否记录访问日志，默认 `false`。开启后每个请求以 Stackdriver 兼容的 JSON 格式输出一条 `Debug` 级别日志（`server/http/requestlog.go`），字段包含方法、URL、状态码、耗时、remote IP 以及 `trace`/`spanId`——注意需要日志器级别为 debug 才能看到。
- `WithMiddleware(middlewares ...Middleware)`：注册自定义中间件，可多次调用叠加。链序见 5.3.5 节。
- `WithTracerProvider(tp trace.TracerProvider)`：OpenTelemetry TracerProvider，用于服务器 instrumentation。为 nil 时使用全局（默认 noop）provider。**provider 的初始化与关闭是调用方的职责**（见 5.3.1 节）。
- `WithMeterProvider(mp metric.MeterProvider)`：OpenTelemetry MeterProvider，为 nil 时使用全局 provider。生命周期同样归调用方。
- `WithPropagator(p propagation.TextMapPropagator)`：从入站请求提取 trace context 使用的 propagator，为 nil 时使用全局 propagator。

### 完整示例

下面的示例最小改自 `_examples/http/main.go`，演示了常用 Options 的组合（otel 选项见 5.3 节）：

```go
package main

import (
	"context"
	"log/slog"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/http"
)

func main() {
	cli := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		router := gohttp.NewServeMux()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			_, _ = rw.Write([]byte("hello lynx"))
		})
		app.Register(http.NewServer(router,
			http.WithAddr(":8080"),
			http.WithHealthCheckers(app.HealthCheckers),
			http.WithLogger(app.Logger("logger", "http-requestlog")),
			http.WithRequestLog(true),
			http.WithMiddleware(latencyMiddleware),
		))
		return nil
	},
		lynx.WithName("http-demo"),
		lynx.WithVersion("1.0.0"),
	)
	cli.Run()
}

// latencyMiddleware 是一个演示中间件：记录请求耗时。取自 _examples/http/main.go。
func latencyMiddleware(next gohttp.Handler) gohttp.Handler {
	return gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Default().InfoContext(r.Context(), "request handled", "path", r.URL.Path, "latency", time.Since(start))
	})
}
```

运行后访问 `http://localhost:8080/` 返回 `hello lynx`，`http://localhost:8080/healthz/liveness` 与 `/healthz/readiness` 返回健康检查结果。

## 5.2 gRPC 服务器

### 创建服务器

gRPC 服务器定义在 `server/grpc/server.go`，与 HTTP 服务器不同，`NewServer` 不接收 handler：

```go
func NewServer(opts ...Option) *Server
```

业务服务通过 `GetServer()` 拿到原生 `*grpc.Server` 后注册（见下文完整示例）。默认值：监听地址 `:9090`（`DefaultGRPCAddr`）、超时 60 秒、日志器 `slog.Default()`。

返回的 `*Server` 实现了 `lynx.Service`（即 `Component` + `CheckHealth() error`，见 4.3 节），`Name()` 为 `"grpc"`。这意味着它会被自动收集进应用的健康检查列表：未在运行时 `CheckHealth` 返回 `grpc.ErrServerStopped`。

### Options 一览

- `WithAddr(addr string)`：监听地址，默认 `:9090`。
- `WithTimeout(timeout time.Duration)`：优雅关闭的超时时间，默认 60 秒。注意它**不是**请求处理超时——gRPC 服务器本身没有读/写超时选项，该值只在 `Stop` 时生效：它是 `GracefulStop` 等待时长的**上限**（调用方 Context 已有更早的 deadline 时取较小者），超时后强制 `Stop()`。
- `WithLogger(l *slog.Logger)`：内置 Logging 拦截器使用的日志器。
- `WithInterceptors(interceptors ...grpc.UnaryServerInterceptor)`：追加自定义一元拦截器，链序见下文。
- `WithServerOptions(options ...grpc.ServerOption)`：透传原生 `grpc.ServerOption`（TLS 凭据、消息大小限制、keepalive、最大并发流等），在内部选项之后应用到 `grpc.NewServer`。例如启用 TLS：`grpc.WithServerOptions(grpc.Creds(creds))`。
- `WithTracerProvider(tp trace.TracerProvider)` / `WithMeterProvider(mp metric.MeterProvider)`：otel provider，传给 `otelgrpc.NewServerHandler` 的 stats handler。为 nil 时使用全局 provider，生命周期归调用方（同 5.3.2 节）。

与 HTTP 服务器相比有两点差异：

- gRPC 服务器没有 `WithPropagator`——otelgrpc 固定使用全局 propagator，需要时通过 `otel.SetTextMapPropagator(...)` 设置；
- gRPC 服务器没有 `WithHealthCheck`——标准健康检查服务是内置的（见下文）。

此外，gRPC 服务器**没有**针对 TLS、keepalive、消息压缩等的一等选项——这些均由
`WithServerOptions` 透传原生 `grpc.ServerOption` 配置（`grpc.Creds`、
`grpc.KeepaliveParams`、`grpc.RPCCompressor` 等），详见上面的选项说明。

### 拦截器

`NewServer` 总是先安装内置拦截器（`server/grpc/interceptor/interceptor.go`），再通过 `WithInterceptors` 追加自定义拦截器。一元 RPC 用 `grpc.ChainUnaryInterceptor` 串联，执行顺序为：

```
Recovery → Logging → 自定义拦截器（按声明顺序） → handler
```

Recovery 置于最外层：链内任意一环（含用户拦截器）的 panic 都能被恢复，不会拖垮整个进程。

流式 RPC 同样安装了 `LoggingStream` 与 `RecoveryStream` 两个内置流式拦截器——gRPC 对流式 handler 的 panic 没有内置保护，不拦截会直接崩溃整个进程。

- `Logging(logger)` / `LoggingStream(logger)`：请求前后各打一条日志，包含 `FullMethod` 与耗时；handler 返回错误时打 `Error` 级别。
- `Recovery()` / `RecoveryStream()`：recover handler（含流式）中的 panic，转换为 `codes.Internal` 错误返回，避免单个请求的 panic 拖垮整个进程。

### 健康检查与反射

- **健康检查**：`NewServer` 时自动注册 `grpc.health.v1` 标准健康检查服务；`Start` 时将服务名 `"grpc"` 与标准的空服务名 `""`（大多数 gRPC 健康探针使用）置为 `SERVING`，`Stop` 时均置为 `NOT_SERVING`。负载均衡器/k8s 可以直接使用标准 gRPC 健康检查协议探测。
- **反射**：`Start` 时自动注册 reflection 服务，因此可以直接用 `grpcurl localhost:9090 list` 之类的工具调试，无需额外配置。

### 完整示例

框架没有提供 `_examples/grpc` 示例，下面的完整程序演示了 `NewServer`、自定义拦截器以及通过 `GetServer()` 注册业务服务（服务描述手写，等价于 protoc 生成代码）：

```go
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/grpc"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

func main() {
	cli := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		srv := grpc.NewServer(
			grpc.WithAddr(":9090"),
			grpc.WithLogger(app.Logger()),
			grpc.WithInterceptors(methodLogInterceptor()),
		)
		// 业务服务通过 GetServer() 拿到原生 *grpc.Server 注册。
		// 实际项目中服务描述由 protoc 生成，这里手写一个最小 Echo 服务作演示。
		srv.GetServer().RegisterService(&echoServiceDesc, &echoService{})
		app.Register(srv)
		return nil
	},
		lynx.WithName("grpc-demo"),
		lynx.WithVersion("1.0.0"),
	)
	cli.Run()
}

// methodLogInterceptor 是一个自定义一元拦截器：记录方法名与耗时。
// 它会在内置的 Logging、Recovery 之后执行（见 5.2 节拦截器链序）。
func methodLogInterceptor() gogrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		slog.InfoContext(ctx, "custom interceptor", "method", info.FullMethod, "duration", time.Since(start))
		return resp, err
	}
}

// ---- 以下为手写的服务描述，等价于 protoc 生成的注册代码 ----

// EchoServer 是演示服务的接口。
type EchoServer interface {
	Say(context.Context, *emptypb.Empty) (*emptypb.Empty, error)
}

type echoService struct{}

func (echoService) Say(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	return req, nil
}

func sayHandler(srv any, ctx context.Context, dec func(any) error, interceptor gogrpc.UnaryServerInterceptor) (any, error) {
	in := new(emptypb.Empty)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EchoServer).Say(ctx, in)
	}
	info := &gogrpc.UnaryServerInfo{Server: srv, FullMethod: "/demo.Echo/Say"}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(EchoServer).Say(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, in, info, handler)
}

var echoServiceDesc = gogrpc.ServiceDesc{
	ServiceName: "demo.Echo",
	HandlerType: (*EchoServer)(nil),
	Methods:     []gogrpc.MethodDesc{{MethodName: "Say", Handler: sayHandler}},
	Streams:     []gogrpc.StreamDesc{},
	Metadata:    "echo.proto",
}
```

运行后可用 `grpcurl -plaintext localhost:9090 list` 查看服务列表（包含 `demo.Echo`、`grpc.health.v1.Health` 与 `grpc.reflection.v1.ServerReflection`）。

## 5.3 可观测性接入

### 5.3.1 开箱即用：contrib/telemetry（框架托管）

可观测性托管在独立 contrib 模块 `github.com/lynx-go/lynx/contrib/telemetry`，以**组件**形式注册，provider 的创建、全局注册与优雅关闭 flush 全部由框架处理：

```go
// 在 setup 回调中：
app.Register(telemetry.New())
```

组件默认创建：

- **TracerProvider**：noop trace exporter（span 直接丢弃）——生产忘配 exporter 不会向 stdout 倒 trace；开发调试用 `telemetry.WithStdoutTrace()` 显式启用 stdout pretty print；
- **MeterProvider**：Prometheus metric reader；
- **propagator**：W3C TraceContext + Baggage 组合。

创建后的 provider 会**自动设置为 otel 全局 provider**（`otel.SetTracerProvider` 等——这是有意的全局副作用，详见包注释），因此服务器无需任何 otel 配置即自动采集——`WithTracerProvider`/`WithMeterProvider`/`WithPropagator` 为 nil 时服务器本就使用全局 provider。应用优雅关闭时，组件的 `Stop` 会自动 flush 并 shutdown provider（日志中可见 `component=otel`），无需手动注册。Init 还会在未显式 `WithResource` 时自动以应用名构建 `service.name` 资源属性，服务名零配置进入 trace/metrics。

> 注意：组件的 `Init` 在注册时同步执行，因此业务指标（`otel.Meter` 创建的 instrument）必须在 `telemetry.New()` 注册**之后**创建，否则拿到的是 noop meter。

Prometheus 指标仍需自行挂载 `/metrics`（见 5.3.4 节）；默认 reader 使用 Prometheus 默认注册表，与 `promhttp.Handler()` 直接兼容。

自定义导出目标通过 `Option` 替换：

```go
telemetry.New(
    telemetry.WithTraceExporter(otlpTraceExporter), // 替换默认 noop（示例见 5.3.3 节）
    telemetry.WithMetricReader(otlpMetricReader),   // 替换默认 Prometheus
    telemetry.WithPropagator(customPropagator),     // 替换默认 TraceContext+Baggage
)
```

- `WithTraceExporter(exporter sdktrace.SpanExporter)`：自定义 trace exporter；
- `WithStdoutTrace()`：开发调试——无自定义 exporter 时使用 stdout pretty print；
- `WithMetricReader(reader sdkmetric.Reader)`：自定义 metric reader（OTLP 等后端 exporter 均实现 `Reader` 接口）；
- `WithPropagator(p propagation.TextMapPropagator)`：自定义传播器。

需要完全掌控 provider（共享实例、精细调参、自定义关闭时机）时，可不使用该组件，走手动路径，见 5.3.2 节。

### 5.3.2 高阶自定义：手动创建 provider

不使用 `contrib/telemetry` 组件时，exporter 与 provider 的初始化、shutdown **都是调用方的职责**——典型的做法是在应用初始化函数里创建 provider，并通过服务器 `WithTracerProvider`/`WithMeterProvider`/`WithPropagator` 传入、把 shutdown 注册进 `OnStop` 钩子：

```go
shutdown, tp, mp, propagator, err := setupOTel()
if err != nil {
	return err
}
app.OnStop(func(ctx context.Context) error {
	return shutdown(ctx)
})
```

一个需要留意的副作用（v1.0 已消除）：HTTP 服务器底层不再使用 gocloud.dev/server 的实现，改为标准库 `http.Server` + otelhttp，传入的 provider 仅用于当前服务器，**不会**被设置为 otel 全局 provider——全局 provider 只能通过 `otel.SetTracerProvider` 等显式设置（或使用 5.3.1 节的托管路径）。

开发调试最方便的是把 span 打到 stdout。下面是一个完整的 `setupOTel` 模板，同时初始化 stdout trace exporter、Prometheus metrics exporter 与 W3C propagator（需要的依赖：`go.opentelemetry.io/otel/exporters/stdout/stdouttrace`、`go.opentelemetry.io/otel/exporters/prometheus`、`go.opentelemetry.io/otel/sdk` 与 `go.opentelemetry.io/otel/sdk/metric`；使用 5.3.1 托管路径时这些依赖框架已内置，无需单独引入）：

```go
func setupOTel() (shutdown func(context.Context) error, tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider, propagator propagation.TextMapPropagator, err error) {
	traceExporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter))

	promExporter, err := prometheus.New()
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, nil, nil, nil, err
	}
	mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(promExporter))

	propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

	shutdown = func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return shutdown, tp, mp, propagator, nil
}
```

### 5.3.3 生产环境：OTLP Exporter

生产环境通常把 span 推给 OTLP collector（Jaeger、Tempo、厂商 APM 等）。框架内置的默认 exporter 不含 OTLP，需要额外引入：

```bash
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
```

初始化 exporter（注意：这里创建的是 **exporter** 而不是 TracerProvider，可以直接交给 5.3.1 的 `telemetry.WithTraceExporter` 由组件托管；手动路径则自行包成 provider）：

```go
// setupOTLPExporter 初始化 OTLP gRPC trace exporter。
func setupOTLPExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // 生产环境应配置 TLS，或改用 HTTP 协议的 otlptracehttp
	)
}
```

托管路径用法：

```go
telemetry.New(
	telemetry.WithTraceExporter(exporter), // exporter 由上面的 setupOTLPExporter 创建
)
```

如果 collector 只暴露 HTTP 端口，把 `otlptracegrpc` 换成 `otlptracehttp`，API 形状一致。endpoint 也支持通过环境变量 `OTEL_EXPORTER_OTLP_ENDPOINT` 配置（不传 `WithEndpoint` 时生效）。metrics 侧同理：`go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc` 的 exporter 实现了 `sdkmetric.Reader`，交给 `telemetry.WithMetricReader` 即可。

### 5.3.4 Prometheus 指标与 /metrics

metrics 一侧的关键点：`go.opentelemetry.io/otel/exporters/prometheus` 的 exporter 本身就是一个 Prometheus registry，把它作为 `Reader` 装进 `MeterProvider`（5.3.1 托管路径的默认 reader 就是它；手动路径见 5.3.2 节模板），再把标准库 `promhttp.Handler()` 挂到路由上即可暴露指标（取自 `_examples/http/main.go`）：

```go
// Note: /metrics is served on the main router for demo simplicity, so
// every Prometheus scrape also flows through the otel instrumentation
// and latencyMiddleware. In production, consider serving it on a
// separate mux or listener to avoid self-referential spans/metrics.
router.Handle("/metrics", promhttp.Handler())
```

注意示例中的提醒：挂在主路由上意味着每次 Prometheus 抓取自己也会产生 span 和指标，生产环境建议把 `/metrics` 放到独立的 mux 或独立的监听端口上。

**业务自定义指标**：托管路径下 provider 已是全局值，业务代码直接用 `otel.Meter` 创建 instrument 即可，采集与导出自动打通（取自 `_examples/http/metrics.go`）：

```go
var (
	helloRequestsCounter metric.Int64Counter
	helloRequestDuration metric.Float64Histogram
)

func initMetrics() error {
	meter := otel.Meter("http-example")
	var err error
	helloRequestsCounter, err = meter.Int64Counter(
		"hello.requests.total",
		metric.WithDescription("total number of requests handled by /"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}
	helloRequestDuration, err = meter.Float64Histogram(
		"hello.request.duration",
		metric.WithDescription("request handling duration of /"),
		metric.WithUnit("s"),
	)
	return err
}
```

处理器里记录指标（`r.Context()` 携带 span 上下文，指标与链路自动关联）：

```go
router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
	start := time.Now()
	helloRequestsCounter.Add(r.Context(), 1)
	defer func() {
		helloRequestDuration.Record(r.Context(), time.Since(start).Seconds())
	}()
	// ...业务处理...
})
```

抓取 `/metrics` 即可看到 `hello_requests_total` 与 `hello_request_duration_seconds`（含 `otel_scope_name` 等标签）。

### 5.3.5 HTTP 中间件链序

`WithMiddleware` 注册的中间件按**声明顺序**应用，先声明的在最外层（`server/http/middleware.go` 的 `chain`）：

```go
// chain wraps h with middlewares, first declared being outermost.
func chain(h http.Handler, middlewares []Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
```

完整的请求处理链由 otelhttp 在最外层注入 otel instrumentation，随后是请求日志中间件，最终链序为：

```
otel instrumentation → request log → WithMiddleware 中间件（声明序） → 业务 handler
```

因此：在自定义中间件和业务 handler 里，`r.Context()` 已经携带了 otel 提取/新建的 SpanContext——这正是 5.3.6 节日志注入 trace_id 的前提；而访问日志（request log）记录的延迟包含自定义中间件的耗时。

### 5.3.6 日志与链路关联：lynx.NewTraceHandler

链路有了，还需要让日志带上 `trace_id`/`span_id` 才能在日志系统里按链路检索。根模块 `logging.go` 提供了一个 slog Handler 装饰器：

```go
func NewTraceHandler(h slog.Handler) slog.Handler
```

它包装任意 `slog.Handler`，当 log 调用的 Context 携带有效的 OpenTelemetry SpanContext 时，自动为记录追加 `trace_id` 与 `span_id` 两个字段。两个使用前提：

1. 打日志必须用带 Context 的方法（`InfoContext`/`ErrorContext` 等），否则装饰器拿不到 SpanContext；
2. Context 里要有有效的 span——HTTP handler 与 gRPC 拦截器链内天然满足（见 5.3.5 节），应用初始化阶段的日志则没有 span，不加字段。

**slog 路线**：直接包装标准库 handler，再 `SetLogger` 给应用：

```go
// newSlogLogger 纯 slog 路线：在任意 slog.Handler 外包一层 NewTraceHandler。
func newSlogLogger() *slog.Logger {
	return slog.New(lynx.NewTraceHandler(slog.NewJSONHandler(os.Stdout, nil)))
}
```

**zap 路线**：`contrib/zap` 通过 `samber/slog-zap` 把 zap 桥接成 slog Handler（见 4.5 节），同样在外层包 `NewTraceHandler` 即可。`MustNewLogger`/`NewSyncableLogger` 返回的 logger 默认没有包装，需要注入 trace 字段时按下面方式自行组装：

```go
// newZapLogger zap 路线：contrib/zap 基于 slog-zap 桥接，同样在外层包 NewTraceHandler。
func newZapLogger() (*slog.Logger, error) {
	zapLogger, err := lynxzap.NewZapLogger("debug")
	if err != nil {
		return nil, err
	}
	handler := slogzap.Option{Level: slog.LevelDebug, Logger: zapLogger}.NewZapHandler()
	return slog.New(lynx.NewTraceHandler(handler)), nil
}
```

其中 `lynxzap` 是 `github.com/lynx-go/lynx/contrib/zap` 的别名，`slogzap` 是 `github.com/samber/slog-zap/v2`。组装出的 logger 传给 `app.SetLogger(...)` 后，组件内所有 `InfoContext` 日志都会携带链路字段；再把它传给 `http.WithLogger`，访问日志（含 `trace`/`spanId` 字段，见 5.1 节 `WithRequestLog`）也走同一条管线。

## 5.4 延伸阅读

本章是教程的最后一章。更多内容可以参考：

- [第 1 章：项目简介](./01-introduction.md) - 回顾 Lynx 的设计理念与核心特性
- [示例代码](../_examples/) - 可编译运行的完整应用示例（HTTP、CLI、boot、pubsub、schedule）
