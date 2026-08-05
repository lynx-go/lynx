package metrics

import (
	"context"
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
	// Init 不使用 app 参数，直接传 nil。
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

	comp.Stop(context.Background())
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
	comp.Stop(context.Background())
}

var _ lynx.Component = new(otelComponent)

type fakeSpanExporter struct{}

func (e *fakeSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *fakeSpanExporter) Shutdown(context.Context) error {
	return nil
}
