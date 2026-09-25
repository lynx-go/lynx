package telemetry

import (
	"context"
	"fmt"
	"time"

	"github.com/lynx-go/lynx"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// FileConfig 是 telemetry 配置段（yaml 键 → mapstructure 字段）的完整 schema。
// 段缺失 / 空段 = 全默认（noop trace + Prometheus metrics + runtime metrics），
// 与 New() 等价。
type FileConfig struct {
	Trace  TraceConfig  `mapstructure:"trace"`
	Metric MetricConfig `mapstructure:"metric"`
}

// TraceConfig 是 trace 管线配置。
type TraceConfig struct {
	// Exporter：noop（默认，span 丢弃）| stdout（开发调试）| otlp（gRPC）。
	Exporter string `mapstructure:"exporter"`
	// SamplingRatio 非 nil 时启用 ParentBased(TraceIDRatioBased(r))：
	// 0 = 不采样、1 = 全采样；省略 = SDK 默认（ParentBased AlwaysSample）。
	SamplingRatio *float64 `mapstructure:"sampling_ratio"`
	// OTLP 是 exporter=otlp 时的连接参数。
	OTLP OTLPConfig `mapstructure:"otlp"`
}

// MetricConfig 是 metric 管线配置。
type MetricConfig struct {
	// Exporter：prometheus（默认，需自行挂载 /metrics）| otlp（gRPC 推送）。
	Exporter string `mapstructure:"exporter"`
	// Interval 是 otlp 推送间隔；0 = SDK 默认（60s）。
	Interval time.Duration `mapstructure:"interval"`
	// OTLP 是 exporter=otlp 时的连接参数。
	OTLP OTLPConfig `mapstructure:"otlp"`
}

// OTLPConfig 是 OTLP/gRPC 连接参数（trace 与 metric 共用）。
type OTLPConfig struct {
	// Endpoint 形如 host:port；空 = SDK 默认（localhost:4317）。
	Endpoint string `mapstructure:"endpoint"`
	// Insecure 为 true 时明文连接（本地 / 集群内采集器常用）；默认 TLS。
	Insecure bool `mapstructure:"insecure"`
	// Headers 随每次导出发送（如 authorization）。
	Headers map[string]string `mapstructure:"headers"`
	// Timeout 单次导出上限；0 = SDK 默认（10s）。
	Timeout time.Duration `mapstructure:"timeout"`
	// Compression：none（默认）| gzip。
	Compression string `mapstructure:"compression"`
}

// NewFromConfig 按 telemetry 段装配托管服务：段缺失 / 空段 = 全默认。
// 非法配置（未知 exporter、采样比例越界、压缩不识别）在装配期报错。
// 配置派生选项先应用，调用方 opts 最后覆盖（显式选项优先的仓库惯例）。
func NewFromConfig(cfg lynx.Config, opts ...Option) (lynx.Service, error) {
	var file FileConfig
	if cfg != nil {
		if err := cfg.UnmarshalKey("telemetry", &file, lynx.WithStrictTypes()); err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
	}
	cfgOpts, err := optionsFromFile(file)
	if err != nil {
		return nil, err
	}
	return New(append(cfgOpts, opts...)...), nil
}

// optionsFromFile 把配置段映射为构造选项（唯一映射点，测试直接断言选项）。
func optionsFromFile(f FileConfig) ([]Option, error) {
	var opts []Option

	switch f.Trace.Exporter {
	case "", "noop":
		// 默认：noop（span 丢弃）。
	case "stdout":
		opts = append(opts, WithStdoutTrace())
	case "otlp":
		traceOpts, err := otlpTraceOptions(f.Trace.OTLP)
		if err != nil {
			return nil, err
		}
		exp, err := otlptracegrpc.New(context.Background(), traceOpts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: trace otlp exporter: %w", err)
		}
		opts = append(opts, WithTraceExporter(exp))
	default:
		return nil, fmt.Errorf("telemetry: unknown trace exporter %q (want noop|stdout|otlp)", f.Trace.Exporter)
	}
	if r := f.Trace.SamplingRatio; r != nil {
		if *r < 0 || *r > 1 {
			return nil, fmt.Errorf("telemetry: trace sampling_ratio %v out of [0,1]", *r)
		}
		opts = append(opts, WithTraceSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*r))))
	}

	switch f.Metric.Exporter {
	case "", "prometheus":
		// 默认：Prometheus（需自行挂载 /metrics）。
	case "otlp":
		metricOpts, err := otlpMetricOptions(f.Metric.OTLP)
		if err != nil {
			return nil, err
		}
		exp, err := otlpmetricgrpc.New(context.Background(), metricOpts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: metric otlp exporter: %w", err)
		}
		readerOpts := []sdkmetric.PeriodicReaderOption{}
		if f.Metric.Interval > 0 {
			readerOpts = append(readerOpts, sdkmetric.WithInterval(f.Metric.Interval))
		}
		opts = append(opts, WithMetricReader(sdkmetric.NewPeriodicReader(exp, readerOpts...)))
	default:
		return nil, fmt.Errorf("telemetry: unknown metric exporter %q (want prometheus|otlp)", f.Metric.Exporter)
	}

	return opts, nil
}

func otlpTraceOptions(c OTLPConfig) ([]otlptracegrpc.Option, error) {
	comp, err := otlpCompression(c.Compression)
	if err != nil {
		return nil, err
	}
	opts := make([]otlptracegrpc.Option, 0, 5)
	if c.Endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(c.Endpoint))
	}
	if c.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(c.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(c.Headers))
	}
	if c.Timeout > 0 {
		opts = append(opts, otlptracegrpc.WithTimeout(c.Timeout))
	}
	if comp != "" {
		opts = append(opts, otlptracegrpc.WithCompressor(comp))
	}
	return opts, nil
}

func otlpMetricOptions(c OTLPConfig) ([]otlpmetricgrpc.Option, error) {
	comp, err := otlpCompression(c.Compression)
	if err != nil {
		return nil, err
	}
	opts := make([]otlpmetricgrpc.Option, 0, 5)
	if c.Endpoint != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(c.Endpoint))
	}
	if c.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(c.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(c.Headers))
	}
	if c.Timeout > 0 {
		opts = append(opts, otlpmetricgrpc.WithTimeout(c.Timeout))
	}
	if comp != "" {
		opts = append(opts, otlpmetricgrpc.WithCompressor(comp))
	}
	return opts, nil
}

// otlpCompression 规范化压缩配置："" / "none" → 不设置；"gzip" → gzip。
func otlpCompression(s string) (string, error) {
	switch s {
	case "", "none":
		return "", nil
	case "gzip":
		return "gzip", nil
	default:
		return "", fmt.Errorf("telemetry: unknown otlp compression %q (want none|gzip)", s)
	}
}
