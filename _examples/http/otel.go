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
