package lynx

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestWithOTelSetsGlobalsAndRegistersShutdown(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	beforeProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
		otel.SetTextMapPropagator(beforeProp)
	})

	o := NewOptions(WithOTel())
	if o.OTel == nil {
		t.Fatal("WithOTel should set OTel options")
	}

	app, err := newLynx(o)
	if err != nil {
		t.Fatalf("newLynx failed: %v", err)
	}
	defer app.Close()
	lynxApp := app.(*lynx)

	if tp := otel.GetTracerProvider(); tp == beforeTP {
		t.Error("TracerProvider was not set to a new global provider")
	}
	if mp := otel.GetMeterProvider(); mp == beforeMP {
		t.Error("MeterProvider was not set to a new global provider")
	}

	var shutdownCalls int
	lynxApp.onStops = append(lynxApp.onStops, func(ctx context.Context) error {
		shutdownCalls++
		return nil
	})
	for _, fn := range lynxApp.onStops {
		if err := fn(context.Background()); err != nil {
			t.Fatalf("on-stop hook failed: %v", err)
		}
	}
	if shutdownCalls != 1 {
		t.Errorf("expected 1 extra on-stop hook, got %d", shutdownCalls)
	}
}

func TestWithOTelCustomProviders(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	mp := sdkmetric.NewMeterProvider()
	defer func() { _ = mp.Shutdown(context.Background()) }()

	o := NewOptions(WithOTel(
		WithOTelTraceExporter(&fakeSpanExporter{}),
		WithOTelMetricReader(sdkmetric.NewManualReader()),
	))
	app, err := newLynx(o)
	if err != nil {
		t.Fatalf("newLynx failed: %v", err)
	}
	defer app.Close()

	if got := otel.GetTracerProvider(); got == beforeTP {
		t.Error("TracerProvider was not replaced")
	}
	if got := otel.GetMeterProvider(); got == beforeMP {
		t.Error("MeterProvider was not replaced")
	}
}

type fakeSpanExporter struct{}

func (e *fakeSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *fakeSpanExporter) Shutdown(context.Context) error {
	return nil
}
