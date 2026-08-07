package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// 业务指标：注册在全局 MeterProvider 上（由 contrib/telemetry 服务设置），
// 经 /metrics（Prometheus）导出。initMetrics 必须在 telemetry.New 服务注册
// 之后调用，否则拿到的是 noop meter。
var (
	helloRequestsCounter metric.Int64Counter
	helloRequestDuration metric.Float64Histogram
)

func initMetrics() error {
	meter := otel.Meter("http-example")
	var err error
	helloRequestsCounter, err = meter.Int64Counter(
		"hello.requests.total",
		metric.WithDescription("total number of requests handled by /"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}
	helloRequestDuration, err = meter.Float64Histogram(
		"hello.request.duration",
		metric.WithDescription("request handling duration of /"),
		metric.WithUnit("s"),
	)
	return err
}
