package lynx

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// OTelOptions 是内置 OTel 初始化的配置项。
type OTelOptions struct {
	traceExporter sdktrace.SpanExporter
	metricReader  sdkmetric.Reader
	propagator    propagation.TextMapPropagator
}

// OTelOption 用于配置内置 OTel 初始化。
type OTelOption func(*OTelOptions)

// WithOTelTraceExporter 设置自定义 trace exporter（默认 stdout，pretty print）。
func WithOTelTraceExporter(exporter sdktrace.SpanExporter) OTelOption {
	return func(o *OTelOptions) {
		o.traceExporter = exporter
	}
}

// WithOTelMetricReader 设置自定义 metric reader（如 OTLP exporter），默认 Prometheus。
func WithOTelMetricReader(reader sdkmetric.Reader) OTelOption {
	return func(o *OTelOptions) {
		o.metricReader = reader
	}
}

// WithOTelPropagator 设置自定义 propagator，默认 TraceContext + Baggage 组合。
func WithOTelPropagator(p propagation.TextMapPropagator) OTelOption {
	return func(o *OTelOptions) {
		o.propagator = p
	}
}

// WithOTel 启用框架托管的 OTel 初始化：创建 TracerProvider 与 MeterProvider、
// 设置为 otel 全局 provider，并在应用优雅关闭时自动 shutdown（flush）。
//
// 开箱即用：默认 stdout trace exporter + Prometheus metric reader +
// TraceContext/Baggage propagator（Prometheus 指标需自行挂载 /metrics，
// 如 promhttp.Handler()，其使用默认注册表与默认 exporter 兼容）。
//
// 需要自定义时通过 OTelOption 替换 exporter/reader/propagator；或放弃本
// 选项走手动路径：自行创建 provider，通过服务器 With* 选项传入并注册
// OnStop 关闭（此时生命周期归调用方）。
func WithOTel(opts ...OTelOption) Option {
	o := &OTelOptions{
		propagator: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
	for _, opt := range opts {
		opt(o)
	}
	return func(options *Options) {
		options.OTel = o
	}
}

// initOTel 创建 provider、设置为 otel 全局值，并注册自动 shutdown 的 OnStop 钩子。
// 未启用 WithOTel 时直接返回 nil。
func (app *lynx) initOTel() error {
	o := app.o.OTel
	if o == nil {
		return nil
	}

	var err error
	if o.traceExporter == nil {
		o.traceExporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return err
		}
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(o.traceExporter))
	otel.SetTracerProvider(tp)

	if o.metricReader == nil {
		o.metricReader, err = prometheus.New()
		if err != nil {
			_ = tp.Shutdown(context.Background())
			return err
		}
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(o.metricReader))
	otel.SetMeterProvider(mp)

	otel.SetTextMapPropagator(o.propagator)

	app.onStops = append(app.onStops, func(ctx context.Context) error {
		var shutdownErrors ShutdownErrors
		shutdownErrors.Add(tp.Shutdown(ctx))
		shutdownErrors.Add(mp.Shutdown(ctx))
		if shutdownErrors.HasErrors() {
			return &shutdownErrors
		}
		return nil
	})
	return nil
}
