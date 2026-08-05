// Package metrics 以组件形式托管 OpenTelemetry 的生命周期：创建
// TracerProvider 与 MeterProvider、设置为 otel 全局值，并在应用停止时
// 自动 flush 与关闭。
//
// 默认导出：stdout trace exporter（pretty print）+ Prometheus metric reader
// + W3C TraceContext/Baggage propagator。Prometheus 指标需自行挂载
// /metrics（如 promhttp.Handler()），其使用默认注册表，与默认 reader 兼容。
package metrics

import (
	"context"
	"errors"
	"log/slog"

	"github.com/lynx-go/lynx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Options 是 metrics 组件的配置项。
type Options struct {
	traceExporter sdktrace.SpanExporter
	metricReader  sdkmetric.Reader
	propagator    propagation.TextMapPropagator
	// res 是附加到 trace/metrics 数据的 OTel Resource（如 service.name）；
	// nil 时使用 SDK 默认资源。
	res *resource.Resource
}

// Option 用于配置 metrics 组件。
type Option func(*Options)

// WithTraceExporter 设置自定义 trace exporter（默认 stdout，pretty print）。
func WithTraceExporter(exporter sdktrace.SpanExporter) Option {
	return func(o *Options) {
		o.traceExporter = exporter
	}
}

// WithMetricReader 设置自定义 metric reader（如 OTLP exporter），默认 Prometheus。
func WithMetricReader(reader sdkmetric.Reader) Option {
	return func(o *Options) {
		o.metricReader = reader
	}
}

// WithPropagator 设置自定义 propagator，默认 TraceContext + Baggage 组合。
func WithPropagator(p propagation.TextMapPropagator) Option {
	return func(o *Options) {
		o.propagator = p
	}
}

// WithResource 设置 OTel Resource（如 service.name 等标准属性），
// nil 时使用 SDK 默认资源。
func WithResource(r *resource.Resource) Option {
	return func(o *Options) {
		o.res = r
	}
}

// New 创建托管 OTel 生命周期的组件：Init 创建 provider 并设置为 otel
// 全局值，Start 阻塞至应用关闭，Stop 自动 flush 并 shutdown。
//
// 业务指标（otel.Meter 创建的 instrument）需在组件注册之后创建，
// 否则拿到的是 noop meter。
func New(opts ...Option) lynx.Component {
	o := &Options{
		propagator: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
	for _, opt := range opts {
		opt(o)
	}
	return &otelComponent{options: o}
}

type otelComponent struct {
	options *Options
	tp      *sdktrace.TracerProvider
	mp      *sdkmetric.MeterProvider
	inited  bool
}

// Name 返回组件名称 "otel"。
func (c *otelComponent) Name() string {
	return "otel"
}

// Init 创建 provider 并设置为 otel 全局值。重复 Init 返回错误：
// 多次注册会覆盖 otel 全局且首个 provider 永不 Shutdown（泄漏）。
func (c *otelComponent) Init(app lynx.App) error {
	if c.inited {
		return errors.New("metrics component already initialized (register once)")
	}
	tp, mp, err := newProviders(c.options)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(c.options.propagator)
	c.tp, c.mp = tp, mp
	c.inited = true
	return nil
}

// Start 阻塞至应用关闭（组件 actor 语义）。
func (c *otelComponent) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop 自动 flush 并关闭 provider。未 Init（provider 为空）时安全返回。
func (c *otelComponent) Stop(ctx context.Context) {
	var shutdownErrors lynx.ShutdownErrors
	if c.tp != nil {
		shutdownErrors.Add(c.tp.Shutdown(ctx))
	}
	if c.mp != nil {
		shutdownErrors.Add(c.mp.Shutdown(ctx))
	}
	if shutdownErrors.HasErrors() {
		slog.ErrorContext(ctx, "otel shutdown errors", "errors", shutdownErrors.Error())
	}
}

// newProviders 按 Options 创建 TracerProvider 与 MeterProvider；
// 未指定 exporter/reader 时使用默认值（stdout trace + Prometheus）。
// 不修改传入的 Options（状态化 Options 是代码味道）。
func newProviders(o *Options) (tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider, err error) {
	traceExporter := o.traceExporter
	if traceExporter == nil {
		traceExporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, err
		}
	}
	traceOpts := []sdktrace.TracerProviderOption{sdktrace.WithBatcher(traceExporter)}
	if o.res != nil {
		traceOpts = append(traceOpts, sdktrace.WithResource(o.res))
	}
	tp = sdktrace.NewTracerProvider(traceOpts...)

	metricReader := o.metricReader
	if metricReader == nil {
		metricReader, err = prometheus.New()
		if err != nil {
			_ = tp.Shutdown(context.Background())
			return nil, nil, err
		}
	}
	metricOpts := []sdkmetric.Option{sdkmetric.WithReader(metricReader)}
	if o.res != nil {
		metricOpts = append(metricOpts, sdkmetric.WithResource(o.res))
	}
	mp = sdkmetric.NewMeterProvider(metricOpts...)
	return tp, mp, nil
}
