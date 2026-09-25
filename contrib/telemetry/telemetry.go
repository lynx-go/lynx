// Package telemetry 以服务形式托管 OpenTelemetry 的生命周期：创建
// TracerProvider 与 MeterProvider、设置为 otel 全局值，并在应用停止时
// 自动 flush 与关闭。
//
// # 全局副作用（有意为之）
//
// Init 会把创建的 provider 设置为 otel 全局值（otel.SetTracerProvider /
// otel.SetMeterProvider / otel.SetTextMapPropagator），此后业务代码与
// server 包（其 provider 参数为 nil 时）都会使用这些全局 provider。
// 这与 server 包"显式注入 provider，不修改全局"的策略互补：server 需要
// 显式传 provider（或从 otel.GetTracerProvider() 自取），本服务则负责
// 创建并托管全局 provider 的生命周期。重复注册（重复 Init）返回错误。
//
// 默认导出：noop trace exporter（span 直接丢弃——生产环境忘配 exporter
// 不会向 stdout 倒 trace；开发调试用 WithStdoutTrace）+ Prometheus metric
// reader + W3C TraceContext/Baggage propagator。Prometheus 指标需自行挂载
// /metrics（如 promhttp.Handler()），其使用默认注册表，与默认 reader 兼容。
//
// Init 在 ctx 非 nil 且未显式 WithResource 时，自动以应用名
// （lynx.Meta(ctx.Context()).Name）构建 service.name 资源属性，
// 服务名零配置进入 trace/metrics。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"sync/atomic"

	"github.com/lynx-go/lynx"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Options 是 telemetry 服务的配置项。
type options struct {
	traceExporter sdktrace.SpanExporter
	// traceSampler 是显式采样器；nil = SDK 默认（ParentBased AlwaysSample）。
	traceSampler sdktrace.Sampler
	metricReader sdkmetric.Reader
	propagator   propagation.TextMapPropagator
	// res 是附加到 trace/metrics 数据的 OTel Resource（如 service.name）；
	// nil 时使用 SDK 默认资源（Init 时可能自动补 service.name）。
	res *resource.Resource
	// stdoutTrace 标记开发调试：无自定义 exporter 时使用 stdout pretty print。
	stdoutTrace bool
	// runtimeMetrics 标记是否注册 Go runtime 指标（goroutine/GC/内存，
	// otel runtime instrument），缺省开启；经 WithoutRuntimeMetrics 关闭。
	runtimeMetrics bool
}

// Option 用于配置 telemetry 服务。
type Option func(*options)

// WithTraceExporter 设置自定义 trace exporter（默认 noop）。
func WithTraceExporter(exporter sdktrace.SpanExporter) Option {
	return func(o *options) {
		o.traceExporter = exporter
	}
}

// WithStdoutTrace 以 stdout pretty print exporter 输出 span，供开发调试。
// 仅在未设置 WithTraceExporter 时生效。
func WithStdoutTrace() Option {
	return func(o *options) {
		o.stdoutTrace = true
	}
}

// WithMetricReader 设置自定义 metric reader（如 OTLP exporter），默认 Prometheus。
func WithMetricReader(reader sdkmetric.Reader) Option {
	return func(o *options) {
		o.metricReader = reader
	}
}

// WithTraceSampler 设置 trace 采样器；未设置时使用 SDK 默认
// （ParentBased AlwaysSample）。比例采样可用
// sdktrace.ParentBased(sdktrace.TraceIDRatioBased(r))，或经 NewFromConfig 的
// telemetry.trace.sampling_ratio 配置。
func WithTraceSampler(s sdktrace.Sampler) Option {
	return func(o *options) {
		if s != nil {
			o.traceSampler = s
		}
	}
}

// WithPropagator 设置自定义 propagator，默认 TraceContext + Baggage 组合。
func WithPropagator(p propagation.TextMapPropagator) Option {
	return func(o *options) {
		o.propagator = p
	}
}

// WithResource 设置 OTel Resource（如 service.name 等标准属性），
// nil 时使用 SDK 默认资源，并在 Init 时自动附加应用名。
func WithResource(r *resource.Resource) Option {
	return func(o *options) {
		o.res = r
	}
}

// WithoutRuntimeMetrics 关闭 Go runtime 指标注册（goroutine/GC/内存）。
// 缺省开启：runtime instrument 挂在 MeterProvider 上随指标管线一并
// 输出，零配置获得进程级可观测基线。注意：进程的 CPU 配额感知
// （容器环境 GOMAXPROCS 修正）由 Go 1.25+ runtime 内建，与本选项无关。
func WithoutRuntimeMetrics() Option {
	return func(o *options) {
		o.runtimeMetrics = false
	}
}

// PrometheusHandler 返回 Prometheus 采集端点（promhttp，默认注册表），供
// server/http 的运维端点一行挂载：
//
//	server := http.NewServer(router,
//		http.WithEndpoint("/metrics", telemetry.PrometheusHandler()))
//
// 与默认 Prometheus metric reader（otel 导出器，写入默认注册表）配套；
// WithEndpoint 的挂载语义保证抓取流量不经过业务中间件与 otel
// instrumentation（不产生自引用指标/日志）。自定义注册表（WithMetricReader
// + prometheus.WithRegisterer）场景请自行使用 promhttp.HandlerFor。
func PrometheusHandler() http.Handler {
	return promhttp.Handler()
}

// New 创建托管 OTel 生命周期的服务：Init 创建 provider 并设置为 otel
// 全局值，Start 阻塞至应用关闭，Stop 自动 flush 并 shutdown。
//
// 业务指标（otel.Meter 创建的 instrument）需在服务注册之后创建，
// 否则拿到的是 noop meter。
func New(opts ...Option) lynx.Service {
	o := &options{
		propagator:     propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		runtimeMetrics: true,
	}
	for _, opt := range opts {
		opt(o)
	}
	return &otelService{options: o}
}

type otelService struct {
	options *options
	tp      *sdktrace.TracerProvider
	mp      *sdkmetric.MeterProvider
	// inited 以 CAS 守卫 Init 的唯一进入：并发 Init 时仅一个调用方创建
	// provider，其余得到 already-initialized 错误（防止双 provider 且
	// 全局被后者覆盖、前者泄漏）。
	inited atomic.Bool
}

// Name 返回服务名称 "otel"。
func (c *otelService) Name() string {
	return "otel"
}

// Init 创建 provider 并设置为 otel 全局值。重复 Init 返回错误：
// 多次注册会覆盖 otel 全局且首个 provider 永不 Shutdown（泄漏）。
func (c *otelService) Init(ctx lynx.AppContext) error {
	// CAS 先于 provider 创建：Load-then-Store 在并发 Init 下会让两个
	// 调用方都通过检查并各自创建 provider，全局被后者覆盖、前者泄漏。
	if !c.inited.CompareAndSwap(false, true) {
		return errors.New("telemetry service already initialized (register once)")
	}
	options := *c.options
	if ctx != nil && options.res == nil {
		// DX 提升：服务名零配置进入 trace/metrics（应用名取自服务环境）。
		options.res = resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(lynx.Meta(ctx.Context()).Name),
		)
	}
	tp, mp, err := newProviders(&options)
	if err != nil {
		// 创建失败回退标志：Init 保持可重试（与旧语义一致），而不是把
		// 服务永久卡在"已初始化"却无 provider 的状态。
		c.inited.Store(false)
		return err
	}
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(options.propagator)
	// Go runtime 指标（goroutine/GC/内存）挂到本服务的 MeterProvider：
	// 在 provider 设置之后注册，业务指标与 runtime 指标同一管线输出。
	if options.runtimeMetrics {
		if err := otelruntime.Start(otelruntime.WithMeterProvider(mp)); err != nil {
			c.inited.Store(false)
			return fmt.Errorf("register runtime metrics: %w", err)
		}
	}
	c.tp, c.mp = tp, mp
	return nil
}

// Start 阻塞至应用关闭（服务 actor 语义）。
func (c *otelService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop 自动 flush 并关闭 provider。未 Init（provider 为空）时安全返回 nil。
// 关闭错误聚合返回，由框架随 Run() 统一上抛。
//
// 已知取舍（AUX-14）：Stop 不复位 otel 全局 provider——与 Init 设置全局
// 对称。Stop 后全局仍指向已关闭的 provider，后续创建的 span/metric 被
// 静默丢弃而非回落 noop。单进程单次生命周期（框架托管、进程即将退出）
// 场景下无碍；同进程重建 telemetry 服务时请自行 otel.SetTracerProvider
// 复位（如切回 noop.NewTracerProvider）。
func (c *otelService) Stop(ctx context.Context) error {
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
//
// 已知取舍（AUX-16）：默认 Prometheus exporter 内部使用 otel 全局
// DefaultRegisterer 注册 collector，且 Stop 不会 unregister——进程内
// 多次创建本服务（重建/反复测试）会让 collector 在全局注册表累积、
// 指标重复上报。请按单实例使用；确需重建的场景改用 WithMetricReader
// 传入自管注册表的 reader。
func newProviders(o *options) (tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider, err error) {
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
	if o.traceSampler != nil {
		traceOpts = append(traceOpts, sdktrace.WithSampler(o.traceSampler))
	}
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
