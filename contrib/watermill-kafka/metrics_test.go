package kafka

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeOffsetReader 是 offsetReader 假件。
type fakeOffsetReader struct {
	partitions map[string][]int32
	hw         map[string]int64 // key: "topic/partition"
	closed     bool
}

func (f *fakeOffsetReader) Partitions(topic string) ([]int32, error) {
	parts, ok := f.partitions[topic]
	if !ok {
		return nil, fmt.Errorf("no partitions for %q", topic)
	}
	return parts, nil
}

func (f *fakeOffsetReader) GetOffset(topic string, partition int32, _ int64) (int64, error) {
	return f.hw[fmt.Sprintf("%s/%d", topic, partition)], nil
}

func (f *fakeOffsetReader) Close() error { f.closed = true; return nil }

// fakeAdmin 是 groupOffsetReader 假件。
type fakeAdmin struct {
	offsets map[string]int64 // key: "topic/partition"；<0 = 未提交
	closed  bool
}

func (f *fakeAdmin) ListConsumerGroupOffsets(_ string, tp map[string][]int32) (*sarama.OffsetFetchResponse, error) {
	resp := &sarama.OffsetFetchResponse{Blocks: map[string]map[int32]*sarama.OffsetFetchResponseBlock{}}
	for topic, parts := range tp {
		resp.Blocks[topic] = map[int32]*sarama.OffsetFetchResponseBlock{}
		for _, p := range parts {
			resp.Blocks[topic][p] = &sarama.OffsetFetchResponseBlock{
				Offset: f.offsets[fmt.Sprintf("%s/%d", topic, p)],
			}
		}
	}
	return resp, nil
}

func (f *fakeAdmin) Close() error { f.closed = true; return nil }

// gaugePoints 从采集结果中取出 lag 指标数据点。
func gaugePoints(t *testing.T, rm metricdata.ResourceMetrics) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != lagMetricName {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %s data type = %T, want Gauge[int64]", m.Name, m.Data)
			}
			return g.DataPoints
		}
	}
	t.Fatalf("metric %s not found in %v", lagMetricName, rm.ScopeMetrics)
	return nil
}

func attrString(t *testing.T, set attribute.Set, key string) string {
	t.Helper()
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		t.Fatalf("attribute %q missing in %v", key, set)
	}
	return v.AsString()
}

// TestLagCollectorCollect：按（物理 topic × 分区 × 消费组）计算
// 高水位 - 已提交 offset；未提交分区跳过；只发布 / 无组条目无点。
func TestLagCollectorCollect(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	gauge, err := mp.Meter(meterName).Int64Gauge(lagMetricName)
	if err != nil {
		t.Fatalf("gauge: %v", err)
	}

	c := newLagCollector(Options{Topics: map[string]TopicOptions{
		"orders": {
			Brokers:  []string{"b1"},
			Topics:   []string{"orders_v1"},
			Consumer: &ConsumerOptions{GroupID: "g1"},
		},
		"publish-only": {Brokers: []string{"b1"}, Topics: []string{"t2"}},
		"no-group":     {Brokers: []string{"b1"}, Topics: []string{"t3"}, Consumer: &ConsumerOptions{}},
	}}, discardLogger())
	client := &fakeOffsetReader{
		partitions: map[string][]int32{"orders_v1": {0, 1}},
		hw:         map[string]int64{"orders_v1/0": 100, "orders_v1/1": 50},
	}
	admin := &fakeAdmin{offsets: map[string]int64{"orders_v1/0": 90, "orders_v1/1": -1}}
	c.newClient = func([]string, *SASLOptions, *TLSOptions) (offsetReader, error) { return client, nil }
	c.newAdmin = func([]string, *SASLOptions, *TLSOptions) (groupOffsetReader, error) { return admin, nil }

	c.collect(context.Background(), gauge, map[string]offsetReader{}, map[string]groupOffsetReader{})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	points := gaugePoints(t, rm)
	if len(points) != 1 {
		t.Fatalf("data points = %d, want 1 (partition 1 uncommitted skipped; publish-only/no-group skipped)", len(points))
	}
	if points[0].Value != 10 {
		t.Fatalf("lag = %d, want 10 (100-90)", points[0].Value)
	}
	if got := attrString(t, points[0].Attributes, "messaging.destination.name"); got != "orders_v1" {
		t.Errorf("destination = %q, want orders_v1", got)
	}
	if got := attrString(t, points[0].Attributes, "messaging.consumer.group.name"); got != "g1" {
		t.Errorf("group = %q, want g1", got)
	}
	if v, ok := points[0].Attributes.Value(attribute.Key("messaging.kafka.partition")); !ok || v.AsInt64() != 0 {
		t.Errorf("partition = %v, want 0", v)
	}
}

// TestLagCollectorSharesClientsPerBrokerGroup：同（brokers × 认证）的多个
// 逻辑 topic 共享采集连接（按 key 缓存）。
func TestLagCollectorSharesClientsPerBrokerGroup(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	gauge, _ := mp.Meter(meterName).Int64Gauge(lagMetricName)

	c := newLagCollector(Options{Topics: map[string]TopicOptions{
		"a": {Brokers: []string{"b1"}, Topics: []string{"ta"}, Consumer: &ConsumerOptions{GroupID: "g"}},
		"b": {Brokers: []string{"b1"}, Topics: []string{"tb"}, Consumer: &ConsumerOptions{GroupID: "g"}},
	}}, discardLogger())
	clientCalls, adminCalls := 0, 0
	client := &fakeOffsetReader{partitions: map[string][]int32{"ta": {0}, "tb": {0}}, hw: map[string]int64{"ta/0": 5, "tb/0": 7}}
	admin := &fakeAdmin{offsets: map[string]int64{"ta/0": 5, "tb/0": 4}}
	c.newClient = func([]string, *SASLOptions, *TLSOptions) (offsetReader, error) { clientCalls++; return client, nil }
	c.newAdmin = func([]string, *SASLOptions, *TLSOptions) (groupOffsetReader, error) { adminCalls++; return admin, nil }

	c.collect(context.Background(), gauge, map[string]offsetReader{}, map[string]groupOffsetReader{})
	if clientCalls != 1 || adminCalls != 1 {
		t.Fatalf("client/admin creations = %d/%d, want 1/1 (shared per broker group)", clientCalls, adminCalls)
	}

	var rm metricdata.ResourceMetrics
	_ = reader.Collect(context.Background(), &rm)
	if points := gaugePoints(t, rm); len(points) != 2 {
		t.Fatalf("data points = %d, want 2", len(points))
	}
}

// TestLagCollectorRunStopsOnCtxCancel：采集循环随 ctx 取消退出并关闭
// 采集连接（生命周期跟随 Start ctx / Stop）。
func TestLagCollectorRunStopsOnCtxCancel(t *testing.T) {
	c := newLagCollector(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"t"}, Consumer: &ConsumerOptions{GroupID: "g"}},
	}}, discardLogger())
	c.interval = 10 * time.Millisecond
	client := &fakeOffsetReader{partitions: map[string][]int32{"t": {0}}, hw: map[string]int64{"t/0": 1}}
	admin := &fakeAdmin{offsets: map[string]int64{"t/0": 1}}
	c.newClient = func([]string, *SASLOptions, *TLSOptions) (offsetReader, error) { return client, nil }
	c.newAdmin = func([]string, *SASLOptions, *TLSOptions) (groupOffsetReader, error) { return admin, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond) // 让首轮采集发生
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector run did not stop on ctx cancel")
	}
	if !client.closed || !admin.closed {
		t.Fatalf("collector connections not closed: client=%v admin=%v", client.closed, admin.closed)
	}
}

// TestMetricsConfigDefaults：默认关闭、30s；配置覆盖 enabled/interval。
func TestMetricsConfigDefaults(t *testing.T) {
	if enabled, interval := (Options{}).metricsConfig(); enabled || interval != 30*time.Second {
		t.Fatalf("defaults = (%v, %v), want (false, 30s)", enabled, interval)
	}
	if enabled, interval := (Options{Metrics: &MetricsOptions{Enabled: boolPtr(true)}}).metricsConfig(); !enabled || interval != 30*time.Second {
		t.Fatalf("enabled = (%v, %v), want (true, 30s)", enabled, interval)
	}
	if enabled, interval := (Options{Metrics: &MetricsOptions{Enabled: boolPtr(true), Interval: 5 * time.Second}}).metricsConfig(); !enabled || interval != 5*time.Second {
		t.Fatalf("interval override = (%v, %v), want (true, 5s)", enabled, interval)
	}
}

// TestOptionsMetricsReservedKey：yaml 的 kafka.metrics 是保留键——映射到
// Metrics 字段而不是逻辑 topic（`metrics` 不能用作 topic 名）。
func TestOptionsMetricsReservedKey(t *testing.T) {
	var opts Options
	err := fromConfigTestConfig(t, `
kafka:
  metrics:
    enabled: false
    interval: 5s
  orders:
    brokers: ["b1"]
    topics: ["orders_v1"]
`).UnmarshalKey("kafka", &opts)
	if err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	tr, err := NewTransport(opts)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if tr.opts.Metrics == nil || tr.opts.Metrics.Enabled == nil || *tr.opts.Metrics.Enabled {
		t.Fatalf("metrics config not decoded: %+v", tr.opts.Metrics)
	}
	if tr.opts.Metrics.Interval != 5*time.Second {
		t.Fatalf("interval = %v, want 5s", tr.opts.Metrics.Interval)
	}
	if len(tr.opts.Topics) != 1 {
		t.Fatalf("topics = %v, want only orders (metrics must be a reserved key)", tr.opts.Topics)
	}
	if _, ok := tr.opts.Topics["orders"]; !ok {
		t.Fatalf("orders topic missing: %v", tr.opts.Topics)
	}
}
