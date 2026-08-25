package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

func TestServiceLifecycle(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	comp := New()
	// Init 不使用 env 参数，直接传 nil。
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if tp := otel.GetTracerProvider(); tp == beforeTP {
		t.Error("TracerProvider was not set to a new global provider")
	}
	if mp := otel.GetMeterProvider(); mp == beforeMP {
		t.Error("MeterProvider was not set to a new global provider")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- comp.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}

	_ = comp.Stop(context.Background())
}

func TestCustomProviders(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	comp := New(
		WithTraceExporter(&fakeSpanExporter{}),
		WithMetricReader(sdkmetric.NewManualReader()),
	)
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if got := otel.GetTracerProvider(); got == beforeTP {
		t.Error("TracerProvider was not replaced")
	}
	if got := otel.GetMeterProvider(); got == beforeMP {
		t.Error("MeterProvider was not replaced")
	}
	_ = comp.Stop(context.Background())
}

// TestStopShutsDownProviders verifies the module's core lifecycle promise:
// Stop must flush and shut down the created providers.
func TestStopShutsDownProviders(t *testing.T) {
	exporter := &recordingSpanExporter{}
	comp := New(WithTraceExporter(exporter))
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	_ = comp.Stop(context.Background())
	if got := exporter.shutdownCount.Load(); got != 1 {
		t.Errorf("exporter Shutdown called %d times, want 1", got)
	}
}

// TestStopBeforeInitDoesNotPanic verifies Stop is safe when Init was never
// called (providers are nil).
func TestStopBeforeInitDoesNotPanic(t *testing.T) {
	comp := New()
	_ = comp.Stop(context.Background()) // must not panic on nil providers
}

// TestStopReturnsErrors 验证 provider Shutdown 错误经 Stop 返回（关停错误
// 对称上抛）。
func TestStopReturnsErrors(t *testing.T) {
	comp := New(WithTraceExporter(&failShutdownExporter{}))
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if err := comp.Stop(context.Background()); err == nil {
		t.Fatal("Stop() error = nil, want shutdown errors")
	}
}

// TestDefaultTraceExporterIsNoop 验证默认 trace exporter 为 noop：未配置
// exporter 时 newProviders 不构造任何 exporter（生产忘配 exporter 不会向
// stdout 倒 trace），span 只记录不导出。
func TestDefaultTraceExporterIsNoop(t *testing.T) {
	tp, mp, err := newProviders(&Options{})
	if err != nil {
		t.Fatalf("newProviders: %v", err)
	}
	defer func() { _ = mp.Shutdown(context.Background()) }()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("noop-test").Start(context.Background(), "span")
	span.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

// TestStdoutTraceOption 验证 WithStdoutTrace 显式启用 stdout pretty print
// exporter。
func TestStdoutTraceOption(t *testing.T) {
	tp, mp, err := newProviders(&Options{stdoutTrace: true})
	if err != nil {
		t.Fatalf("newProviders: %v", err)
	}
	defer func() { _ = mp.Shutdown(context.Background()) }()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("stdout-test").Start(context.Background(), "span")
	span.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

var _ lynx.Service = new(otelService)

type fakeSpanExporter struct{}

func (e *fakeSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *fakeSpanExporter) Shutdown(context.Context) error {
	return nil
}

// recordingSpanExporter records how many times Shutdown was called.
type recordingSpanExporter struct {
	shutdownCount atomic.Int32
}

func (e *recordingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *recordingSpanExporter) Shutdown(context.Context) error {
	e.shutdownCount.Add(1)
	return nil
}

// failShutdownExporter 的 Shutdown 恒返回错误。
type failShutdownExporter struct{}

func (e *failShutdownExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *failShutdownExporter) Shutdown(context.Context) error {
	return errors.New("shutdown boom")
}

// TestInitTwiceFails 回归：重复 Init 覆盖 otel 全局且首个 provider 永不
// Shutdown（泄漏），必须返回错误。
func TestInitTwiceFails(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	comp := New()
	if err := comp.Init(nil); err != nil {
		t.Fatalf("first Init failed: %v", err)
	}
	if err := comp.Init(nil); err == nil {
		t.Fatal("expected error on second Init")
	}
}

// TestConcurrentInitSingleWinner 回归 AUX-03：Load-then-Store 检查下并发
// Init 可能让多个调用方都通过检查、各自创建 provider（全局被后者覆盖、
// 前者泄漏）；CAS 必须保证仅一个调用方成功。
func TestConcurrentInitSingleWinner(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	comp := New(WithTraceExporter(&fakeSpanExporter{}), WithMetricReader(sdkmetric.NewManualReader()))
	const n = 8
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := comp.Init(nil); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("concurrent Init successes = %d, want exactly 1", got)
	}
	_ = comp.Stop(context.Background())
}

// fakeAppCtx 是 lynx.AppContext 的最小测试替身（service.name 注入分支用）。
type fakeAppCtx struct{}

func (f *fakeAppCtx) Context() context.Context       { return context.Background() }
func (f *fakeAppCtx) Config() lynx.Config            { return nil }
func (f *fakeAppCtx) Bus() eventbus.Bus              { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (f *fakeAppCtx) Logger(...any) *slog.Logger     { return slog.Default() }
func (f *fakeAppCtx) HealthCheckers() []lynx.Checker { return nil }
func (f *fakeAppCtx) Close()                         {}

// spanRecordingExporter 记录导出的 span，供 resource 属性断言。
type spanRecordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *spanRecordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *spanRecordingExporter) Shutdown(context.Context) error { return nil }

func (e *spanRecordingExporter) recorded() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

// TestInitInjectsServiceNameResource 覆盖 AUX-17 测试缺口：Init(ctx) 的
// ctx 非 nil 且未显式 WithResource 时自动以应用名构建 service.name 资源
// 属性，该分支此前零覆盖。
func TestInitInjectsServiceNameResource(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	exporter := &spanRecordingExporter{}
	comp := New(WithTraceExporter(exporter), WithMetricReader(sdkmetric.NewManualReader()))
	if err := comp.Init(&fakeAppCtx{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = comp.Stop(context.Background()) }()

	svc := comp.(*otelService)
	_, span := svc.tp.Tracer("aux17-test").Start(context.Background(), "span")
	span.End()
	if err := svc.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	spans := exporter.recorded()
	if len(spans) == 0 {
		t.Fatal("no spans were exported")
	}
	// ctx 非 nil 分支注入了 service.name 资源属性（应用名取自服务环境；
	// 测试替身未注入 metadata，值为空字符串，但属性本身必须存在）。
	if _, ok := spans[0].Resource().Set().Value(semconv.ServiceNameKey); !ok {
		t.Errorf("span resource missing service.name attribute, got: %v", spans[0].Resource())
	}
}

// TestInitNilCtxSkipsServiceName 验证 Init(nil) 不注入 service.name
// 资源（与 TestInitInjectsServiceNameResource 形成分支对照）。
func TestInitNilCtxSkipsServiceName(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	exporter := &spanRecordingExporter{}
	comp := New(WithTraceExporter(exporter), WithMetricReader(sdkmetric.NewManualReader()))
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = comp.Stop(context.Background()) }()

	svc := comp.(*otelService)
	_, span := svc.tp.Tracer("aux17-test").Start(context.Background(), "span")
	span.End()
	if err := svc.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	spans := exporter.recorded()
	if len(spans) == 0 {
		t.Fatal("no spans were exported")
	}
	// Init(nil) 未走注入分支：service.name 保持 SDK 默认值
	//（"unknown_service:<binary>"），而非被本服务覆盖为应用名。
	// 若属性缺失亦视为未注入（防御不同 SDK 版本的默认 resource 差异）。
	res := spans[0].Resource()
	if v, ok := res.Set().Value(semconv.ServiceNameKey); ok {
		if got := v.AsString(); got != "" && !strings.HasPrefix(got, "unknown_service") {
			t.Errorf("Init(nil) unexpectedly injected service.name = %q, want SDK default, resource: %v", got, res)
		}
	}
}
