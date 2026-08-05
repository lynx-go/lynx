package lynx

import (
	"context"
	"log/slog"

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

	tp, mp, err := newOTelProviders(o)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tp)
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

// newOTelProviders 按 OTelOptions 创建 TracerProvider 与 MeterProvider；
// 未指定 exporter/reader 时使用默认值（stdout trace + Prometheus）。
func newOTelProviders(o *OTelOptions) (tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider, err error) {
	if o.traceExporter == nil {
		o.traceExporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, err
		}
	}
	tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(o.traceExporter))

	if o.metricReader == nil {
		o.metricReader, err = prometheus.New()
		if err != nil {
			_ = tp.Shutdown(context.Background())
			return nil, nil, err
		}
	}
	mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(o.metricReader))
	return tp, mp, nil
}

// OTelComponent 是以组件形式托管的 OTel 生命周期：Init 创建 provider 并
// 设置为 otel 全局值，Start 阻塞至应用关闭，Stop 自动 flush 并 shutdown。
// 与 WithOTel（Builder 选项）等价，区别是生命周期纳入组件调度；
// 注意业务指标（otel.Meter 创建 instrument）需在 OTelComponent 注册之后创建，
// 否则拿到的是 noop meter。
type OTelComponent struct {
	options *OTelOptions
	tp      *sdktrace.TracerProvider
	mp      *sdkmetric.MeterProvider
}

// NewOTelComponent 创建托管 OTel 生命周期的组件，选项与 WithOTel 相同。
func NewOTelComponent(opts ...OTelOption) Component {
	o := &OTelOptions{
		propagator: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
	for _, opt := range opts {
		opt(o)
	}
	return &OTelComponent{options: o}
}

// Name 返回组件名称 "otel"。
func (c *OTelComponent) Name() string {
	return "otel"
}

// Init 创建 provider 并设置为 otel 全局值。
func (c *OTelComponent) Init(app App) error {
	tp, mp, err := newOTelProviders(c.options)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(c.options.propagator)
	c.tp, c.mp = tp, mp
	return nil
}

// Start 阻塞至应用关闭（组件 actor 语义）。
func (c *OTelComponent) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop 自动 flush 并关闭 provider。
func (c *OTelComponent) Stop(ctx context.Context) {
	var shutdownErrors ShutdownErrors
	shutdownErrors.Add(c.tp.Shutdown(ctx))
	shutdownErrors.Add(c.mp.Shutdown(ctx))
	if shutdownErrors.HasErrors() {
		slog.ErrorContext(ctx, "otel shutdown errors", "errors", shutdownErrors.Error())
	}
}
