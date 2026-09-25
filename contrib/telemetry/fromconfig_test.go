package telemetry

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// fromConfigTestConfig 构造 telemetry 段配置（YAML）。
func fromConfigTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return lynx.NewViperConfig(v)
}

// TestNewFromConfigDefaults：段缺失 = 全默认（noop trace + Prometheus metrics）。
func TestNewFromConfigDefaults(t *testing.T) {
	svc, err := NewFromConfig(fromConfigTestConfig(t, ""))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	o := svc.(*otelService).options
	if o.traceExporter != nil || o.stdoutTrace || o.traceSampler != nil {
		t.Fatalf("trace defaults not applied: %+v", o)
	}
	if o.metricReader != nil {
		t.Fatal("metric reader should stay nil (Prometheus default built in newProviders)")
	}
}

// TestNewFromConfigTraceStdoutAndSampling：stdout 与比例采样映射
// （ParentBased(TraceIDRatioBased)）。
func TestNewFromConfigTraceStdoutAndSampling(t *testing.T) {
	svc, err := NewFromConfig(fromConfigTestConfig(t, `
telemetry:
  trace:
    exporter: stdout
    sampling_ratio: 0.25
`))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	o := svc.(*otelService).options
	if !o.stdoutTrace {
		t.Fatal("stdoutTrace not set")
	}
	if o.traceSampler == nil {
		t.Fatal("traceSampler not set")
	}
	desc := o.traceSampler.Description()
	if !strings.Contains(desc, "TraceIDRatioBased") || !strings.Contains(desc, "0.25") {
		t.Fatalf("sampler description = %q, want ParentBased TraceIDRatioBased 0.25", desc)
	}
}

// TestNewFromConfigOTLP：OTLP trace / metric 装配（懒连接，无需采集器）；
// interval / 压缩 / 头 / 超时映射不报错。
func TestNewFromConfigOTLP(t *testing.T) {
	svc, err := NewFromConfig(fromConfigTestConfig(t, `
telemetry:
  trace:
    exporter: otlp
    otlp:
      endpoint: 127.0.0.1:4317
      insecure: true
      headers: {authorization: "Bearer x"}
      timeout: 3s
      compression: gzip
  metric:
    exporter: otlp
    interval: 5s
    otlp:
      endpoint: 127.0.0.1:4317
      insecure: true
`))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	o := svc.(*otelService).options
	if o.traceExporter == nil {
		t.Fatal("trace otlp exporter not wired")
	}
	if o.metricReader == nil {
		t.Fatal("metric otlp reader not wired")
	}
}

// TestNewFromConfigValidation：非法配置在装配期报错（启动即失败）。
func TestNewFromConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown trace exporter", "telemetry:\n  trace:\n    exporter: kafka\n", "unknown trace exporter"},
		{"unknown metric exporter", "telemetry:\n  metric:\n    exporter: influx\n", "unknown metric exporter"},
		{"sampling ratio too large", "telemetry:\n  trace:\n    sampling_ratio: 1.5\n", "out of"},
		{"sampling ratio negative", "telemetry:\n  trace:\n    sampling_ratio: -0.1\n", "out of"},
		{"unknown compression", "telemetry:\n  trace:\n    exporter: otlp\n    otlp:\n      compression: snappy\n", "unknown otlp compression"},
		{"strict types", "telemetry:\n  trace:\n    otlp:\n      headers: 42\n", "telemetry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFromConfig(fromConfigTestConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

// TestNewFromConfigCallerOptionOverride：调用方 opts 最后应用（覆盖配置值）。
func TestNewFromConfigCallerOptionOverride(t *testing.T) {
	svc, err := NewFromConfig(fromConfigTestConfig(t, `
telemetry:
  trace:
    sampling_ratio: 0.25
`), WithTraceSampler(sdktrace.AlwaysSample()))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	o := svc.(*otelService).options
	if o.traceSampler == nil || !strings.Contains(o.traceSampler.Description(), "AlwaysOn") {
		t.Fatalf("caller sampler not applied: %v", o.traceSampler)
	}
}
