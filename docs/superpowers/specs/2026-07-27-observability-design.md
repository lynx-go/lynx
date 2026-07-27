# Phase B 可观测性设计

> 日期：2026-07-27 ｜ 对应 ROADMAP Phase B（v0.9.0）

## 背景与目标

Lynx 当前裸 `http.Handler` + gocloud `server` 包装，gRPC 有 interceptor 链。otel 依赖（`otelhttp`、`otel`、`otel/metric`、`otel/trace`）已作为 gocloud 间接依赖存在于根模块 go.mod。

Phase B 目标：OpenTelemetry tracing 接入 HTTP/gRPC、Prometheus metrics、HTTP 最小中间件抽象、日志 trace 上下文注入。

## 已确认的设计决策

1. **只做插装，Provider 用户传入**：框架通过 Option 接收 `TracerProvider`/`MeterProvider`/`Propagator`；exporter 初始化、生命周期、关闭完全由用户掌控。框架零强制 exporter 依赖。
2. **otel metric 统一到底**：metrics 复用 otelhttp/otelgrpc 自带插装，用户将 Prometheus exporter（`go.opentelemetry.io/otel/exporters/prometheus`）挂到自己的 `MeterProvider` 即可，不自写 client_golang 中间件。
3. **日志只做 trace 上下文注入**：一个 `slog.Handler` 装饰器统一注入 `trace_id`/`span_id`，slog 默认线与 contrib/zap 线共用，保证两条线行为一致。不动其余字段规范。
4. **HTTP 中间件用 WithMiddleware Option**：与 gRPC `WithInterceptors` 对称，零破坏性。

## 关键现状事实

- gocloud `server.Options` 已支持 `TraceProvider`/`MetricsProvider`/`TraceTextMapPropagator`，其 `init()` 会把它们设为 otel 全局 provider，并将用户 handler 包装为 `otelhttp.NewHandler(h, "", otelhttp.WithPublicEndpoint())`（使用全局 provider）。
- 因此 HTTP 侧**不需要自己包 otelhttp**（否则会双重插装），只需透传 provider 选项。
- gocloud 的包装顺序：`otelhttp → requestlog → 用户 handler`；健康检查端点在 otelhttp 之外，不产生 span。

## 详细设计

### 1. HTTP tracing/metrics（`server/http/server.go`）

`Options` 新增三个字段及 Option 函数：

```go
TracerProvider trace.TracerProvider          // WithTracerProvider
MeterProvider  metric.MeterProvider          // WithMeterProvider
Propagator     propagation.TextMapPropagator // WithPropagator
```

`Start` 中透传：

```go
opts := &server.Options{
    HealthChecks:            healthChecks,
    TraceProvider:           s.o.TracerProvider,
    MetricsProvider:         s.o.MeterProvider,
    TraceTextMapPropagator:  s.o.Propagator,
    Driver:                  driver,
}
```

三者均为零值时 gocloud 不设置全局 provider，otelhttp 使用全局 noop，**行为与现状完全一致**。

依赖变动：根模块 go.mod 中 `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`、`go.opentelemetry.io/otel`、`otel/metric`、`otel/trace` 由 indirect 转 direct。

### 2. HTTP 中间件抽象（`server/http`）

```go
// middleware.go（新文件）
type Middleware func(http.Handler) http.Handler

func WithMiddleware(middlewares ...Middleware) Option
```

`Start` 中按声明顺序包装（第一个声明的最靠外）：

```go
h := s.handler
for i := len(s.o.Middlewares) - 1; i >= 0; i-- {
    h = s.o.Middlewares[i](h)
}
// h 传给 gocloud server.New
```

最终调用链：`otelhttp → requestlog → 用户中间件（声明顺序）→ 用户 handler`。

### 3. gRPC tracing/metrics（`server/grpc/server.go`）

`Options` 新增：

```go
TracerProvider trace.TracerProvider // WithTracerProvider
MeterProvider  metric.MeterProvider // WithMeterProvider
```

`NewServer` 中挂载 stats handler（otelgrpc v0.62 现代 API，同时产出 trace 与 metric）：

```go
shOptions := []otelgrpc.Option{}
if options.TracerProvider != nil { shOptions = append(shOptions, otelgrpc.WithTracerProvider(options.TracerProvider)) }
if options.MeterProvider != nil { shOptions = append(shOptions, otelgrpc.WithMeterProvider(options.MeterProvider)) }
grpcOpts := []grpc.ServerOption{
    grpc.StatsHandler(otelgrpc.NewServerHandler(shOptions...)),
    grpc.ChainUnaryInterceptor(interceptors...),
}
```

新依赖：`go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`（与 otelhttp 同版本线 v0.62.0）。

现有 interceptor 链（Logging、Recovery、用户 interceptors）不动。

### 4. 日志 trace 上下文注入（根模块 `logging.go`，新文件）

```go
// NewTraceHandler 返回一个 slog.Handler 装饰器，
// 当日志调用携带含有效 SpanContext 的 context 时，
// 自动为记录附加 trace_id 与 span_id 字段。
func NewTraceHandler(h slog.Handler) slog.Handler
```

实现要点：

- `Handle(ctx, r)`：用 `trace.SpanContextFromContext(ctx)`，若 `IsValid()` 则 `r.AddAttrs(slog.String("trace_id", ...), slog.String("span_id", ...))` 后委托。
- `Enabled`/`WithAttrs`/`WithGroup` 正确委托并返回包装后的 handler（`WithAttrs`/`WithGroup` 必须返回新的 TraceHandler 包装实例，否则装饰器会丢失）。
- 仅依赖 `go.opentelemetry.io/otel/trace`（已在根模块，转 direct）。

两条线用法：

- slog 默认线：`slog.SetDefault(slog.New(lynx.NewTraceHandler(baseHandler)))`
- zap 线：`slog.New(lynx.NewTraceHandler(slogzap.Option{...}.NewZapHandler()))`

contrib/zap **不改代码**，因其产出的是 `slog.Handler`，可在其外层套同一个装饰器。

### 5. 示例（`_examples/http`）

演示最小接入（不改变示例默认行为，otel 初始化独立成函数）：

- `stdouttrace` exporter 初始化 TracerProvider
- `prometheus` exporter 初始化 MeterProvider，并在 router 上挂 `promhttp.Handler()` 暴露 `/metrics`
- `http.WithTracerProvider(...)`、`http.WithMeterProvider(...)`、`http.WithPropagator(...)`
- `WithMiddleware` 演示一个记录耗时的自定义中间件

示例 go.mod 新增：`go.opentelemetry.io/otel/sdk`、`sdk/metric`、`exporters/stdout/stdouttrace`、`exporters/prometheus`、`github.com/prometheus/client_golang`（promhttp）。

### 6. 测试

- `logging_test.go`（根模块）：TraceHandler 有/无 span ctx、无效 SpanContext 不加字段、WithAttrs/WithGroup 委托后装饰器仍生效。
- `server/http/server_test.go`：中间件执行顺序（声明序 = 外到内）；provider 透传验证——用 `tracetest.NewSpanRecorder` + `sdktrace.NewTracerProvider` 传入 `WithTracerProvider`，启动真实 server 发一个请求，断言产出一个 HTTP span。
- `server/grpc/server_test.go`：StatsHandler 挂载后 RPC 产生 span（同样用 `tracetest.NewSpanRecorder`，仅测试依赖）。

## 错误处理

无新错误路径。所有插装在 provider 缺失时 noop-safe；`NewTraceHandler(nil)` 视为编程错误，直接传入会导致 slog panic——与标准库 `slog.New(nil)` 行为一致，不额外防御。

## 兼容性

- 不修改任何现有导出符号签名；`NewServer`、`NewRouter`、interceptor 行为不变。
- 全部新 API 为新增 Option/函数，零值安全。

## 非目标（YAGNI）

- 不做 exporter 初始化/生命周期管理（用户职责）。
- 不做 client_golang 原生中间件。
- 不做完整日志字段规范（service_name 等已有 zap 线处理，不统一）。
- 不做 kafka/pubsub 的 trace 传播（后续 Phase 再议）。
