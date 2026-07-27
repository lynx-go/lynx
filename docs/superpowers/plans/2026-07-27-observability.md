# Phase B 可观测性 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 Lynx 的 HTTP/gRPC 服务器接入 OpenTelemetry tracing/metrics（Provider 用户传入）、提供 HTTP 最小中间件抽象、提供统一的日志 trace_id/span_id 注入装饰器。

**Architecture:** HTTP 侧复用 gocloud.dev/server 内置的 otelhttp 插装，仅透传 Provider 选项；gRPC 侧挂 otelgrpc stats handler；日志注入用根模块的 `slog.Handler` 装饰器，slog/zap 两条线共用。

**Tech Stack:** Go 1.24.2, OpenTelemetry v1.37.0（otel/otel metric/otel trace），otelhttp v0.62.0，otelgrpc v0.62.0，gocloud.dev v0.45.0。

**Spec:** `docs/superpowers/specs/2026-07-27-observability-design.md`（已批准）

## Global Constraints

- 仓库根：`D:/Codes/lynx-go/lynx`，多模块 workspace（go.work，本地已存在但被 gitignore，不要提交它）。
- 只用标准库测试（无 testify）；测试必须过 `go test -race`。
- 每个 Task 完成后 `go vet ./...` 与 `golangci-lint run ./...`（v2.8.0，本地已装）必须干净。
- 不修改任何现有导出符号签名；所有新 API 为零值安全的 Option/函数。
- 不做 exporter 初始化/生命周期管理（用户职责）；contrib/zap 不改代码。
- commit message 用 conventional commits，英文小写开头（参照 git log 风格）。
- 测试文件与生产文件放同一 package（内部测试），沿用现有 `server/http/server_test.go`、`server/grpc/server_test.go` 中的 helper（`waitForDial`、`waitRunning`、`rawCodec` 等已存在，优先复用，重复定义会编译冲突）。

---

### Task 1: 根模块 TraceHandler（日志 trace 注入）

**Files:**
- Create: `logging.go`
- Test: `logging_test.go`

**Interfaces:**
- Produces: `func NewTraceHandler(h slog.Handler) slog.Handler` — 后续 Task 5 示例与 zap 线文档依赖此签名。

- [ ] **Step 1: 写失败的测试**

创建 `logging_test.go`：

```go
package lynx

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func newRecordingHandler() (*bytes.Buffer, slog.Handler) {
	buf := &bytes.Buffer{}
	return buf, slog.NewJSONHandler(buf, nil)
}

func validSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
		TraceFlags: trace.FlagsSampled,
	})
}

func TestTraceHandlerAddsTraceContext(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewTraceHandler(base)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger := slog.New(h)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"trace_id":"`+sc.TraceID().String()+`"`) {
		t.Errorf("log output missing trace_id, got: %s", out)
	}
	if !strings.Contains(out, `"span_id":"`+sc.SpanID().String()+`"`) {
		t.Errorf("log output missing span_id, got: %s", out)
	}
}

func TestTraceHandlerWithoutSpan(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewTraceHandler(base))
	logger.InfoContext(context.Background(), "hello")

	out := buf.String()
	if strings.Contains(out, "trace_id") || strings.Contains(out, "span_id") {
		t.Errorf("log output should not contain trace fields, got: %s", out)
	}
}

func TestTraceHandlerInvalidSpanContext(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewTraceHandler(base))
	// Zero SpanContext is not valid; no fields should be added.
	ctx := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if strings.Contains(out, "trace_id") {
		t.Errorf("invalid span context should not add trace_id, got: %s", out)
	}
}

func TestTraceHandlerWithAttrsKeepsDecoration(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewTraceHandler(base).WithAttrs([]slog.Attr{slog.String("component", "test")})
	logger := slog.New(h)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"trace_id"`) {
		t.Errorf("trace_id lost after WithAttrs, got: %s", out)
	}
	if !strings.Contains(out, `"component":"test"`) {
		t.Errorf("WithAttrs attribute missing, got: %s", out)
	}
}

func TestTraceHandlerWithGroupKeepsDecoration(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewTraceHandler(base).WithGroup("g")
	logger := slog.New(h)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"trace_id"`) {
		t.Errorf("trace_id lost after WithGroup, got: %s", out)
	}
}

func TestTraceHandlerEnabledDelegates(t *testing.T) {
	base := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewTraceHandler(base)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled should delegate to the wrapped handler (Info disabled at Warn level)")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled should delegate to the wrapped handler (Error enabled at Warn level)")
	}
}
```

- [ ] **Step 2: 运行测试确认编译失败**

Run: `go test -run TestTraceHandler ./ 2>&1 | head -5`
Expected: FAIL — `undefined: NewTraceHandler`

- [ ] **Step 3: 实现 logging.go**

创建 `logging.go`：

```go
package lynx

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler is a slog.Handler decorator that injects trace_id and span_id
// from the log call's context when it carries a valid SpanContext.
type traceHandler struct {
	next slog.Handler
}

// NewTraceHandler returns a slog.Handler that wraps h, automatically adding
// trace_id and span_id attributes to records logged with a context that
// carries a valid OpenTelemetry SpanContext. Wrap both plain slog handlers
// and zap-backed slog handlers (contrib/zap) with it for consistent fields.
func NewTraceHandler(h slog.Handler) slog.Handler {
	return traceHandler{next: h}
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{next: h.next.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{next: h.next.WithGroup(name)}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test -race -run TestTraceHandler ./ -v 2>&1 | tail -15`
Expected: 6 个测试全部 PASS

- [ ] **Step 5: vet + lint**

Run: `go vet ./... && golangci-lint run ./...`
Expected: 无输出 / `0 issues.`

- [ ] **Step 6: Commit**

```bash
git add logging.go logging_test.go
git commit -m "feat: add slog trace handler injecting trace_id and span_id"
```

---

### Task 2: server/http 透传 TracerProvider/MeterProvider/Propagator

**Files:**
- Modify: `server/http/server.go`（Options 结构体、With* 函数、Start 中 server.Options 组装）
- Test: `server/http/server_test.go`

**Interfaces:**
- Consumes: 现有 `type Options struct` / `type Option func(*Options)` / `NewServer(handler http.Handler, opts ...Option) *Server`
- Produces: `WithTracerProvider(tp trace.TracerProvider) Option`、`WithMeterProvider(mp metric.MeterProvider) Option`、`WithPropagator(p propagation.TextMapPropagator) Option` — Task 5 示例依赖这三个签名。

- [ ] **Step 1: 准备测试依赖**

```bash
go get go.opentelemetry.io/otel/sdk@v1.37.0
```
（`go.opentelemetry.io/otel/sdk` v1.37.0 已在 go.sum 中，go get 会把它提为 direct 测试依赖。）

- [ ] **Step 2: 写失败的测试**

在 `server/http/server_test.go` 追加（import 需新增 `sdktrace "go.opentelemetry.io/otel/sdk/trace"`、`"go.opentelemetry.io/otel/sdk/trace/tracetest"`；`waitForDial` 复用现有 helper）：

```go
func TestServerTracingProducesSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Grab a free port, then release it for the server to bind.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		w.WriteHeader(gohttp.StatusNoContent)
	}), WithAddr(addr), WithTracerProvider(tp))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)

	resp, err := gohttp.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, gohttp.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}

	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected at least one span from the HTTP request, got none")
	}
	var found bool
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindServer {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a server-kind span, got: %v", spans)
	}
}
```

注意：`server/http/server_test.go` 现有 import 中 net/http 的别名是 `gohttp`，otel trace 用 `"go.opentelemetry.io/otel/trace"`（断言 SpanKind 用）。

- [ ] **Step 3: 运行测试确认失败**

Run: `go test -run TestServerTracingProducesSpan ./server/http/ 2>&1 | head -5`
Expected: FAIL — `undefined: WithTracerProvider`（编译错误）

- [ ] **Step 4: 实现**

`server/http/server.go` 改动三处：

import 块新增：

```go
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
```

`Options` 结构体新增字段：

```go
type Options struct {
	Addr           string
	Timeout        time.Duration
	HealthCheck    lynx.HealthCheckFunc
	Logger         *slog.Logger
	RequestLog     bool
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
}
```

新增 Option 函数（放在 `WithRequestLog` 之后）：

```go
// WithTracerProvider sets the OpenTelemetry TracerProvider used by the
// server's instrumentation. When nil, the global (noop by default) provider
// is used. The provider's lifecycle (init, shutdown) is the caller's
// responsibility.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

// WithMeterProvider sets the OpenTelemetry MeterProvider used by the server's
// instrumentation. When nil, the global (noop by default) provider is used.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *Options) {
		o.MeterProvider = mp
	}
}

// WithPropagator sets the propagator used to extract trace context from
// incoming requests. When nil, the global propagator is used.
func WithPropagator(p propagation.TextMapPropagator) Option {
	return func(o *Options) {
		o.Propagator = p
	}
}
```

`Start` 中的 `server.Options` 组装改为：

```go
	opts := &server.Options{
		HealthChecks:           healthChecks,
		TraceProvider:          s.o.TracerProvider,
		MetricsProvider:        s.o.MeterProvider,
		TraceTextMapPropagator: s.o.Propagator,
		//TraceTextMapPropagator: sdserver.NewTextMapPropagator(),
		Driver: driver,
	}
```

（删掉那行旧的注释掉的 `//TraceTextMapPropagator: sdserver.NewTextMapPropagator()`。）

- [ ] **Step 5: 运行测试确认通过，并跑全量**

Run: `go test -race -count=1 ./server/http/ 2>&1 | tail -3 && go mod tidy && go test -race -count=1 ./... 2>&1 | tail -8`
Expected: 全部 ok；`go mod tidy` 后 otel 相关依赖从 indirect 转 direct

- [ ] **Step 6: vet + lint**

Run: `go vet ./... && golangci-lint run ./...`
Expected: `0 issues.`

- [ ] **Step 7: Commit**

```bash
git add server/http/server.go server/http/server_test.go go.mod go.sum
git commit -m "feat(http): pass through otel tracer/meter providers and propagator"
```

---

### Task 3: server/http 最小中间件抽象

**Files:**
- Create: `server/http/middleware.go`
- Modify: `server/http/server.go`（Options 增加 Middlewares 字段；Start 中包装 handler）
- Test: `server/http/middleware_test.go`

**Interfaces:**
- Consumes: Task 2 的 `Options` / `Option` / `NewServer`
- Produces: `type Middleware func(http.Handler) http.Handler`、`WithMiddleware(middlewares ...Middleware) Option`、`chain(h http.Handler, middlewares []Middleware) http.Handler`（包内私有）— Task 5 示例依赖前两者。

- [ ] **Step 1: 写失败的测试**

创建 `server/http/middleware_test.go`：

```go
package http

import (
	"context"
	"errors"
	"net"
	gohttp "net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestChainAppliesMiddlewareInDeclarationOrder(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next gohttp.Handler) gohttp.Handler {
			return gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
				order = append(order, name+":before")
				next.ServeHTTP(w, r)
				order = append(order, name+":after")
			})
		}
	}
	handler := chain(gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		order = append(order, "handler")
	}), []Middleware{mw("mw1"), mw("mw2")})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(gohttp.MethodGet, "/", nil))

	want := []string{"mw1:before", "mw2:before", "handler", "mw2:after", "mw1:after"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestWithMiddlewareAccumulates(t *testing.T) {
	o := Options{}
	noop := func(next gohttp.Handler) gohttp.Handler { return next }
	WithMiddleware(noop)(&o)
	WithMiddleware(noop, noop)(&o)
	if len(o.Middlewares) != 3 {
		t.Errorf("len(Middlewares) = %d, want 3", len(o.Middlewares))
	}
}

func TestServerAppliesMiddleware(t *testing.T) {
	mw := func(next gohttp.Handler) gohttp.Handler {
		return gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
			w.Header().Set("X-Middleware", "applied")
			next.ServeHTTP(w, r)
		})
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		w.WriteHeader(gohttp.StatusNoContent)
	}), WithAddr(addr), WithMiddleware(mw))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)

	resp, err := gohttp.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	if got := resp.Header.Get("X-Middleware"); got != "applied" {
		t.Errorf("X-Middleware header = %q, want %q", got, "applied")
	}

	srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, gohttp.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}
}
```

（`waitForDial` 复用 server_test.go 中现有 helper；同 package 内可直接调用。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test -run 'TestChain|TestWithMiddleware|TestServerAppliesMiddleware' ./server/http/ 2>&1 | head -5`
Expected: FAIL — `undefined: chain` / `undefined: WithMiddleware` / `unknown field Middlewares`

- [ ] **Step 3: 实现 middleware.go**

创建 `server/http/middleware.go`：

```go
package http

import "net/http"

// Middleware wraps an http.Handler, typically to add cross-cutting behavior
// such as logging, authentication, or custom metrics.
type Middleware func(http.Handler) http.Handler

// WithMiddleware registers middlewares applied to the server's handler in
// declaration order: the first declared middleware is the outermost. The
// final chain is: otel instrumentation -> request log -> middlewares -> handler.
func WithMiddleware(middlewares ...Middleware) Option {
	return func(o *Options) {
		o.Middlewares = append(o.Middlewares, middlewares...)
	}
}

// chain wraps h with middlewares, first declared being outermost.
func chain(h http.Handler, middlewares []Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
```

- [ ] **Step 4: server.go 接线**

`Options` 结构体追加字段：

```go
	Middlewares []Middleware
```

`Start` 中，把

```go
	hs := server.New(s.handler, opts)
```

改为：

```go
	hs := server.New(chain(s.handler, s.o.Middlewares), opts)
```

- [ ] **Step 5: 运行测试确认通过，并跑全量**

Run: `go test -race -count=1 ./server/http/ 2>&1 | tail -3`
Expected: ok

- [ ] **Step 6: vet + lint**

Run: `go vet ./... && golangci-lint run ./...`
Expected: `0 issues.`

- [ ] **Step 7: Commit**

```bash
git add server/http/middleware.go server/http/middleware_test.go server/http/server.go
git commit -m "feat(http): add minimal middleware abstraction via WithMiddleware"
```

---

### Task 4: server/grpc 接入 otelgrpc stats handler

**Files:**
- Modify: `server/grpc/server.go`（Options、NewServer）
- Test: `server/grpc/server_test.go`

**Interfaces:**
- Consumes: 现有 `Options` / `NewServer(opts ...Option) *Server`；`rawCodec{}`（server_test.go 已有）
- Produces: `WithTracerProvider(tp trace.TracerProvider) Option`、`WithMeterProvider(mp metric.MeterProvider) Option`（grpc 包内，与 http 包同名签名）

- [ ] **Step 1: 准备依赖**

```bash
go get go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc@v0.62.0
```

- [ ] **Step 2: 写失败的测试**

在 `server/grpc/server_test.go` 追加（import 新增 `sdktrace "go.opentelemetry.io/otel/sdk/trace"`、`"go.opentelemetry.io/otel/sdk/trace/tracetest"`、`"strings"`；`rawCodec`、`waitRunning` 复用现有）：

```go
func TestServerTracingProducesSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	addr := freeAddr(t)
	s := NewServer(WithAddr(addr), WithTracerProvider(tp))
	s.server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Ping",
		HandlerType: (*interface{})(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Call",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				m := new(struct{})
				if err := dec(m); err != nil {
					return nil, err
				}
				return m, nil
			},
		}},
	}, struct{}{})

	go func() { _ = s.Start(context.Background()) }()
	waitRunning(t, s)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Invoke(context.Background(), "/test.Ping/Call", &struct{}{}, &struct{}{}, grpc.ForceCodec(rawCodec{})); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	s.Stop(context.Background())

	spans := recorder.Ended()
	var found bool
	for _, sp := range spans {
		if strings.Contains(sp.Name(), "test.Ping/Call") {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(spans))
		for _, sp := range spans {
			names = append(names, sp.Name())
		}
		t.Errorf("expected a span for test.Ping/Call, got spans: %v", names)
	}
}
```

如果 `freeAddr(t)` 在 server_test.go 中不存在（检查现有 helper，可能叫别的名字或需内联），用这段：

```go
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test -run TestServerTracingProducesSpan ./server/grpc/ 2>&1 | head -5`
Expected: FAIL — `undefined: WithTracerProvider`

- [ ] **Step 4: 实现**

`server/grpc/server.go`：

import 新增：

```go
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
```

`Options` 新增字段：

```go
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
```

新增 Option 函数（放在 `WithInterceptors` 之后）：

```go
// WithTracerProvider sets the OpenTelemetry TracerProvider used by the
// server's stats handler. When nil, the global (noop by default) provider is
// used. The provider's lifecycle is the caller's responsibility.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

// WithMeterProvider sets the OpenTelemetry MeterProvider used by the server's
// stats handler. When nil, the global (noop by default) provider is used.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *Options) {
		o.MeterProvider = mp
	}
}
```

`NewServer` 中 `grpcOpts` 组装改为：

```go
	statsOpts := []otelgrpc.Option{}
	if options.TracerProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithTracerProvider(options.TracerProvider))
	}
	if options.MeterProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithMeterProvider(options.MeterProvider))
	}
	grpcOpts := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(statsOpts...)),
		grpc.ChainUnaryInterceptor(
			interceptors...,
		),
	}
```

- [ ] **Step 5: 运行测试确认通过，并跑全量**

Run: `go test -race -count=1 ./server/grpc/... 2>&1 | tail -3 && go mod tidy && go test -race -count=1 ./... 2>&1 | tail -8`
Expected: 全部 ok

- [ ] **Step 6: vet + lint**

Run: `go vet ./... && golangci-lint run ./...`
Expected: `0 issues.`

- [ ] **Step 7: Commit**

```bash
git add server/grpc/server.go server/grpc/server_test.go go.mod go.sum
git commit -m "feat(grpc): add otel tracing and metrics via otelgrpc stats handler"
```

---

### Task 5: _examples/http 可观测性接入演示

**Files:**
- Create: `_examples/http/otel.go`
- Modify: `_examples/http/main.go`、`server/http/server.go` 无关，不动
- Modify: `_examples/go.mod`（go get 自动更新）

**Interfaces:**
- Consumes: Task 1 `lynx.NewTraceHandler`、Task 2 `http.WithTracerProvider/WithMeterProvider/WithPropagator`、Task 3 `http.WithMiddleware`
- Produces: 无（示例为终点）

- [ ] **Step 1: 准备依赖**

```bash
cd _examples
go get go.opentelemetry.io/otel/sdk@v1.37.0 go.opentelemetry.io/otel/sdk/metric@v1.37.0 go.opentelemetry.io/otel/exporters/stdout/stdouttrace@v1.37.0 go.opentelemetry.io/otel/exporters/prometheus@v1.37.0 github.com/prometheus/client_golang@latest
cd ..
```

- [ ] **Step 2: 创建 _examples/http/otel.go**

```go
package main

import (
	"context"

	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

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
		return nil, nil, nil, nil, err
	}
	mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(promExporter))

	propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

	shutdown = func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return err
		}
		return mp.Shutdown(ctx)
	}
	return shutdown, tp, mp, propagator, nil
}
```

- [ ] **Step 3: main.go 接线**

`_examples/http/main.go` 中，在 `router := http.NewRouter()` 之后、`http.NewServer` 之前插入（import 新增 `"github.com/prometheus/client_golang/prometheus/promhttp"`、`"log/slog"`、`go.opentelemetry.io/otel/trace` 不需要——main 里只用 lynx/http 包符号）：

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

		router.Handle("/metrics", promhttp.Handler())
```

`http.NewServer` 调用改为：

```go
		if err := app.Hooks(lynx.Components(http.NewServer(router,
			http.WithAddr(addr),
			http.WithHealthCheck(app.HealthCheckFunc()),
			http.WithLogger(app.Logger("logger", "http-requestlog")),
			http.WithTracerProvider(tp),
			http.WithMeterProvider(mp),
			http.WithPropagator(propagator),
			http.WithMiddleware(latencyMiddleware),
		))); err != nil {
			return err
		}
```

并在文件底部新增演示中间件（需要 `"time"` 已 import；`app` 不在作用域内，用包级演示即可）：

```go
// latencyMiddleware is a demo lynx http.Middleware logging request latency.
func latencyMiddleware(next gohttp.Handler) gohttp.Handler {
	return gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Default().InfoContext(r.Context(), "request handled", "path", r.URL.Path, "latency", time.Since(start))
	})
}
```

注意：`slog.Default().InfoContext` 的 ctx 里带 span context——如果用户用 `lynx.NewTraceHandler` 包装了默认 handler，这条日志会自动带 trace_id/span_id，正好演示两条线一致性（示例中不强求接入 TraceHandler，保持示例最小）。

- [ ] **Step 4: 编译 + 测试 + lint**

Run: `cd _examples && go build ./... && go vet ./... && golangci-lint run ./... && cd ..`
Expected: 全部通过，`0 issues.`

- [ ] **Step 5: Commit**

```bash
git add _examples/http/main.go _examples/http/otel.go _examples/go.mod _examples/go.sum
git commit -m "docs(examples): demonstrate otel tracing, prometheus metrics and middleware in http example"
```

---

### Task 6: 收尾验证 + ROADMAP 勾选

**Files:**
- Modify: `ROADMAP.md`（Phase B 四个条目勾选）

**Interfaces:**
- Consumes: Task 1-5 全部产物
- Produces: 无

- [ ] **Step 1: 全模块验证**

Run:

```bash
go build ./... && go vet ./... && go test -race -count=1 ./... 2>&1 | tail -8
(cd _examples && go build ./... && go vet ./... && go test ./... 2>&1 | tail -3)
golangci-lint run ./...
(cd _examples && golangci-lint run ./...)
for d in contrib/kafka contrib/pubsub contrib/schedule contrib/zap; do (cd $d && go build ./... && golangci-lint run ./...); done
```

Expected: 全部通过，所有模块 `0 issues.`

- [ ] **Step 2: ROADMAP.md 勾选 Phase B**

将 ROADMAP.md 中 Phase B 四个 `- [ ]` 改为 `- [x]`：

```markdown
- [x] OpenTelemetry tracing 接入 HTTP/gRPC（go.mod 已有 otel 间接依赖，转为显式支持）
- [x] Prometheus metrics 中间件
- [x] HTTP 侧最小中间件抽象（前置设计决策：当前 HTTP 直接裸 `http.Handler`，metrics/tracing 需要挂载点）
- [x] 统一日志字段规范（trace_id 注入等），zap/slog 两条线行为一致
```

- [ ] **Step 3: Commit**

```bash
git add ROADMAP.md
git commit -m "docs: mark phase B observability items complete in roadmap"
```
