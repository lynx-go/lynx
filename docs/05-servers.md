# 5. 服务器与可观测性

Lynx 在 `server/http` 与 `server/grpc` 两个包中提供了开箱即用的服务器组件，它们都实现了第 4 章介绍的 `Component` 接口，可以直接通过 `lynx.Components` 注册进应用。本章先逐一介绍两个服务器的全部配置项，再讲解可观测性接入：OpenTelemetry trace/metrics 的 provider 初始化、Prometheus 指标暴露、HTTP 中间件链序，以及日志与链路的关联（`lynx.NewTraceHandler`）。

## 5.1 HTTP 服务器

### 创建服务器

HTTP 服务器定义在 `server/http/server.go`，通过 `NewServer` 创建：

```go
func NewServer(handler http.Handler, opts ...Option) *Server
```

第一个参数是标准的 `http.Handler`。框架同时提供了一个 `NewRouter()` 辅助函数，返回标准库的 `*http.ServeMux`——Lynx 不绑定任何第三方路由，想换 chi、gin 的 mux 只需把对应的 `http.Handler` 传进来即可。

默认值（`server/http/server.go` 顶部的常量）：

- 监听地址 `:8080`（`DefaultHTTPAddr`）
- 超时 60 秒（`DefaultTimeout`）
- 日志器 `slog.Default()`

返回的 `*Server` 实现了 `lynx.Component`，`Name()` 为 `"http"`，注册方式与其他组件一致。

### Options 一览

全部 Options 定义在 `server/http/server.go` 与 `server/http/middleware.go`：

- `WithAddr(addr string)`：监听地址，默认 `:8080`。
- `WithTimeout(timeout time.Duration)`：超时时间，默认 60 秒。该值会同时设置为底层 `http.Server` 的 `ReadHeaderTimeout`、`ReadTimeout` 和 `WriteTimeout`；传入 0 或负数则不设置（保持底层默认值）。
- `WithHealthCheck(hc lynx.HealthCheckFunc)`：健康检查函数。传入后服务器自动暴露两个端点（由底层 gocloud.dev 服务器提供）：`/healthz/liveness` 恒返回 200，`/healthz/readiness` 依次调用所有收集到的检查器。通常直接传 `app.HealthCheckFunc()`，收集规则见 2.5 节与 4.3 节。两个端点始终注册；不传该 Option 只是就绪检查列表为空，此时 `/healthz/readiness` 恒返回 200。
- `WithLogger(l *slog.Logger)`：请求日志使用的日志器，默认 `slog.Default()`。
- `WithRequestLog(requestLog bool)`：是否记录访问日志，默认 `false`。开启后每个请求以 Stackdriver 兼容的 JSON 格式输出一条 `Debug` 级别日志（`server/http/requestlog.go`），字段包含方法、URL、状态码、耗时、remote IP 以及 `trace`/`spanId`——注意需要日志器级别为 debug 才能看到。
- `WithMiddleware(middlewares ...Middleware)`：注册自定义中间件，可多次调用叠加。链序见 5.3.5 节。
- `WithTracerProvider(tp trace.TracerProvider)`：OpenTelemetry TracerProvider，用于服务器 instrumentation。为 nil 时使用全局（默认 noop）provider。**provider 的初始化与关闭是调用方的职责**（见 5.3.1 节）。
- `WithMeterProvider(mp metric.MeterProvider)`：OpenTelemetry MeterProvider，为 nil 时使用全局 provider。生命周期同样归调用方。
- `WithPropagator(p propagation.TextMapPropagator)`：从入站请求提取 trace context 使用的 propagator，为 nil 时使用全局 propagator。

### 完整示例

下面的示例最小改自 `_examples/http/main.go`，演示了常用 Options 的组合（otel 相关三个 Option 的取值见 5.3 节）：

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
		router := http.NewRouter()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			_, _ = rw.Write([]byte("hello lynx"))
		})
		return app.Hooks(lynx.Components(http.NewServer(router,
			http.WithAddr(":8080"),
			http.WithHealthCheck(app.HealthCheckFunc()),
			http.WithLogger(app.Logger("logger", "http-requestlog")),
			http.WithRequestLog(true),
			http.WithMiddleware(latencyMiddleware),
		)))
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

返回的 `*Server` 实现了 `lynx.ServerLike`（即 `Component` + `CheckHealth() error`，见 4.3 节），`Name()` 为 `"grpc"`。这意味着它会被自动收集进应用的健康检查列表：未在运行时 `CheckHealth` 返回 `grpc.ErrServerStopped`。

### Options 一览

- `WithAddr(addr string)`：监听地址，默认 `:9090`。
- `WithTimeout(timeout time.Duration)`：优雅关闭的超时时间，默认 60 秒。注意它**不是**请求处理超时——gRPC 服务器本身没有读/写超时选项，该值只在 `Stop` 时生效：若传入的 Context 没有 deadline，则以它为上限等待 `GracefulStop`，超时后强制 `Stop()`。
- `WithLogger(l *slog.Logger)`：内置 Logging 拦截器使用的日志器。
- `WithInterceptors(interceptors ...grpc.UnaryServerInterceptor)`：追加自定义一元拦截器，链序见下文。
- `WithTracerProvider(tp trace.TracerProvider)` / `WithMeterProvider(mp metric.MeterProvider)`：otel provider，传给 `otelgrpc.NewServerHandler` 的 stats handler。为 nil 时使用全局 provider，生命周期归调用方（同 5.3.1 节）。

与 HTTP 服务器相比有两点差异：

- gRPC 服务器没有 `WithPropagator`——otelgrpc 固定使用全局 propagator，需要时通过 `otel.SetTextMapPropagator(...)` 设置；
- gRPC 服务器没有 `WithHealthCheck`——标准健康检查服务是内置的（见下文）。

### 拦截器

`NewServer` 总是先安装两个内置拦截器（`server/grpc/interceptor/interceptor.go`），再通过 `WithInterceptors` 追加自定义拦截器，最终用 `grpc.ChainUnaryInterceptor` 串联，执行顺序为：

```
Logging → Recovery → 自定义拦截器（按声明顺序） → handler
```

- `Logging(logger)`：请求前后各打一条日志，包含 `FullMethod` 与耗时；handler 返回错误时打 `Error` 级别。
- `Recovery()`：recover handler 中的 panic，转换为 `codes.Internal` 错误返回，避免单个请求的 panic 拖垮整个进程。

### 健康检查与反射

- **健康检查**：`NewServer` 时自动注册 `grpc.health.v1` 标准健康检查服务；`Start` 时将服务名 `"grpc"` 置为 `SERVING`，`Stop` 时置为 `NOT_SERVING`。负载均衡器/k8s 可以直接使用标准 gRPC 健康检查协议探测。
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
		return app.Hooks(lynx.Components(srv))
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

### 5.3.1 职责划分：Provider 由用户侧持有

Lynx 的服务器组件只**消费** otel provider：通过 `WithTracerProvider`/`WithMeterProvider`/`WithPropagator` 传给服务器即可，框架从不创建也不关闭它们。**exporter 与 provider 的初始化、 shutdown 都是用户侧的职责**——典型的做法是在应用初始化函数里创建 provider，并把 shutdown 注册进 `OnStop` 钩子（取自 `_examples/http/main.go`）：

```go
shutdown, tp, mp, propagator, err := setupOTel()
if err != nil {
	return err
}
// Provider lifecycle belongs to the caller: shut them down when the
// app stops, not when this setup function returns.
if err := app.Hooks(lynx.OnStop(func(ctx context.Context) error {
	return shutdown(ctx)
})); err != nil {
	return err
}
```

这样 provider 的存活周期覆盖整个应用运行期，且在优雅关闭时被正确 flush。

一个需要留意的副作用：HTTP 服务器底层 gocloud.dev 在 `Start` 时会把传入的非 nil TracerProvider/MeterProvider/Propagator **同时设置为 otel 全局 provider**（即替你调用了 `otel.SetTracerProvider` 等）。单服务进程里这通常正合预期（业务代码可以直接用全局 tracer）；但如果你已自行设置过全局 provider、或同一进程跑多个服务器，要意识到后启动的服务器会覆盖全局值。

### 5.3.2 开发环境：stdouttrace

开发调试时最方便的是把 span 打到 stdout。下面的 `setupOTel` 完整取自 `_examples/http/otel.go`，同时初始化了 stdout trace exporter、Prometheus metrics exporter 与 W3C propagator：

```go
// setupOTel initializes a stdout trace exporter and a Prometheus metrics
// exporter for demonstration purposes. In production, initialize exporters
// in your own bootstrap and pass the providers to the lynx server options;
// their lifecycle stays with the caller.
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

需要的依赖：`go.opentelemetry.io/otel/exporters/stdout/stdouttrace`、`go.opentelemetry.io/otel/exporters/prometheus`、`go.opentelemetry.io/otel/sdk` 与 `go.opentelemetry.io/otel/sdk/metric`。

### 5.3.3 生产环境：OTLP Exporter

生产环境通常把 span 推给 OTLP collector（Jaeger、Tempo、厂商 APM 等）。需要额外引入 exporter 依赖：

```bash
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
```

初始化代码（注意返回的 `shutdown` 同样应注册进 `OnStop` 钩子，见 5.3.1 节）：

```go
// setupTracerProvider 初始化 OTLP gRPC exporter 与 TracerProvider。
// 返回的 shutdown 应在应用停止时调用（例如注册进 lynx.OnStop 钩子）。
func setupTracerProvider(ctx context.Context, endpoint string) (tp *sdktrace.TracerProvider, shutdown func(context.Context) error, err error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // 生产环境应配置 TLS，或改用 HTTP 协议的 otlptracehttp
	)
	if err != nil {
		return nil, nil, err
	}
	tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	return tp, tp.Shutdown, nil
}
```

得到的 `tp` 直接传给 `http.WithTracerProvider(tp)` 或 `grpc.WithTracerProvider(tp)` 即可。如果 collector 只暴露 HTTP 端口，把 `otlptracegrpc` 换成 `otlptracehttp`，API 形状一致。endpoint 也支持通过环境变量 `OTEL_EXPORTER_OTLP_ENDPOINT` 配置（不传 `WithEndpoint` 时生效）。

### 5.3.4 Prometheus 指标与 /metrics

metrics 一侧的关键点：`go.opentelemetry.io/otel/exporters/prometheus` 的 exporter 本身就是一个 Prometheus registry，把它作为 `Reader` 装进 `MeterProvider`（见 5.3.2 节的 `setupOTel`），再把标准库 `promhttp.Handler()` 挂到路由上即可暴露指标（取自 `_examples/http/main.go`）：

```go
// Note: /metrics is served on the main router for demo simplicity, so
// every Prometheus scrape also flows through the otel instrumentation
// and latencyMiddleware. In production, consider serving it on a
// separate mux or listener to avoid self-referential spans/metrics.
router.Handle("/metrics", promhttp.Handler())
```

注意示例中的提醒：挂在主路由上意味着每次 Prometheus 抓取自己也会产生 span 和指标，生产环境建议把 `/metrics` 放到独立的 mux 或独立的监听端口上。

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

完整的请求处理链由 gocloud.dev 服务器在最外层注入 otel instrumentation 与请求日志，最终链序为：

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
