package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestComponentLifecycle(t *testing.T) {
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

var _ lynx.Component = new(otelComponent)

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
