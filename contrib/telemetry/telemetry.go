// Package telemetry 以组件形式托管 OpenTelemetry 的生命周期：创建
// TracerProvider 与 MeterProvider、设置为 otel 全局值，并在应用停止时
// 自动 flush 与关闭。
//
// # 全局副作用（有意为之）
//
// Init 会把创建的 provider 设置为 otel 全局值（otel.SetTracerProvider /
// otel.SetMeterProvider / otel.SetTextMapPropagator），此后业务代码与
// server 包（其 provider 参数为 nil 时）都会使用这些全局 provider。
// 这与 server 包"显式注入 provider，不修改全局"的策略互补：server 需要
// 显式传 provider（或从 otel.GetTracerProvider() 自取），本组件则负责
// 创建并托管全局 provider 的生命周期。重复注册（重复 Init）返回错误。
//
// 默认导出：noop trace exporter（span 直接丢弃——生产环境忘配 exporter
// 不会向 stdout 倒 trace；开发调试用 WithStdoutTrace）+ Prometheus metric
// reader + W3C TraceContext/Baggage propagator。Prometheus 指标需自行挂载
// /metrics（如 promhttp.Handler()），其使用默认注册表，与默认 reader 兼容。
//
// Init 在 ctx 非 nil 且未显式 WithResource 时，自动以应用名
//（NameFromContext(ctx.Context())）构建 service.name 资源属性，
// 服务名零配置进入 trace/metrics。
package telemetry

import (
	"context"
	"errors"

	"github.com/lynx-go/lynx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Options 是 telemetry 组件的配置项。
type Options struct {
	traceExporter sdktrace.SpanExporter
	metricReader  sdkmetric.Reader
	propagator    propagation.TextMapPropagator
	// res 是附加到 trace/metrics 数据的 OTel Resource（如 service.name）；
	// nil 时使用 SDK 默认资源（Init 时可能自动补 service.name）。
	res *resource.Resource
	// stdoutTrace 标记开发调试：无自定义 exporter 时使用 stdout pretty print。
	stdoutTrace bool
}

// Option 用于配置 telemetry 组件。
type Option func(*Options)

// WithTraceExporter 设置自定义 trace exporter（默认 noop）。
func WithTraceExporter(exporter sdktrace.SpanExporter) Option {
	return func(o *Options) {
		o.traceExporter = exporter
	}
}

// WithStdoutTrace 以 stdout pretty print exporter 输出 span，供开发调试。
// 仅在未设置 WithTraceExporter 时生效。
func WithStdoutTrace() Option {
	return func(o *Options) {
		o.stdoutTrace = true
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
// nil 时使用 SDK 默认资源，并在 Init 时自动附加应用名。
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
func (c *otelComponent) Init(ctx lynx.AppContext) error {
	if c.inited {
		return errors.New("telemetry component already initialized (register once)")
	}
	options := *c.options
	if ctx != nil && options.res == nil {
		// DX 提升：服务名零配置进入 trace/metrics（应用名取自组件环境）。
		options.res = resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(lynx.NameFromContext(ctx.Context())),
		)
	}
	tp, mp, err := newProviders(&options)
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

// Stop 自动 flush 并关闭 provider。未 Init（provider 为空）时安全返回 nil。
// 关闭错误聚合返回，由框架随 Run() 统一上抛。
func (c *otelComponent) Stop(ctx context.Context) error {
	var shutdownErrors lynx.ShutdownErrors
	if c.tp != nil {
		shutdownErrors.Add(c.tp.Shutdown(ctx))
	}
	if c.mp != nil {
		shutdownErrors.Add(c.mp.Shutdown(ctx))
	}
	if shutdownErrors.HasErrors() {
		return &shutdownErrors
	}
	return nil
}

// newProviders 按 Options 创建 TracerProvider 与 MeterProvider；
// 未指定 exporter/reader 时使用默认值（noop trace + Prometheus）。
// 不修改传入的 Options（状态化 Options 是代码味道）。
func newProviders(o *Options) (tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider, err error) {
	traceOpts := []sdktrace.TracerProviderOption{}
	if o.traceExporter != nil {
		traceOpts = append(traceOpts, sdktrace.WithBatcher(o.traceExporter))
	} else if o.stdoutTrace {
		exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, err
		}
		traceOpts = append(traceOpts, sdktrace.WithBatcher(exporter))
	}
	// 无 exporter：noop（span 直接丢弃），生产忘配 exporter 不污染 stdout。
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
