package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const runtimeScope = "go.opentelemetry.io/contrib/instrumentation/runtime"

// collectScopeNames 从 ManualReader 收集一轮指标，返回全部 scope 名。
func collectScopeNames(t *testing.T, r *sdkmetric.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	scopes := make(map[string]bool, len(rm.ScopeMetrics))
	for _, sm := range rm.ScopeMetrics {
		scopes[sm.Scope.Name] = true
	}
	return scopes
}

func resetOtelGlobals(t *testing.T) {
	t.Helper()
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(beforeTP)
		otel.SetMeterProvider(beforeMP)
	})
}

// TestRuntimeMetricsDefaultOn：缺省注册 Go runtime 指标（goroutine/GC/
// 内存），经 metric reader 可见。
func TestRuntimeMetricsDefaultOn(t *testing.T) {
	resetOtelGlobals(t)
	reader := sdkmetric.NewManualReader()
	comp := New(WithMetricReader(reader))
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if !collectScopeNames(t, reader)[runtimeScope] {
		t.Errorf("runtime metrics scope %q not collected, want default on", runtimeScope)
	}
}

// TestRuntimeMetricsDisabled：WithoutRuntimeMetrics 后不注册。
func TestRuntimeMetricsDisabled(t *testing.T) {
	resetOtelGlobals(t)
	reader := sdkmetric.NewManualReader()
	comp := New(WithMetricReader(reader), WithoutRuntimeMetrics())
	if err := comp.Init(nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if collectScopeNames(t, reader)[runtimeScope] {
		t.Errorf("runtime metrics scope %q collected, want disabled", runtimeScope)
	}
}
