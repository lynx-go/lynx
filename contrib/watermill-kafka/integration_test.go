//go:build integration

package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/lynxtest"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// startKafka 启动 confluent-local 容器并返回 broker 列表；用例结束终止容器。
// Docker / 镜像不可用时跳过（容器启动失败即视为环境缺失）。
func startKafka(t *testing.T) []string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.5.0",
		tckafka.WithClusterID("lynx-it"))
	if err != nil {
		t.Skipf("kafka container unavailable: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	brokers, err := container.Brokers(ctx)
	if err != nil {
		t.Fatalf("brokers: %v", err)
	}
	return brokers
}

// TestIntegrationFanOut 对真实 Kafka（testcontainers）验证 v1.16 消费模型：
// 同一事件的两个 handler 共享一条 transport 订阅（消费组 / 成员数只来自
// transport 配置）并各自收到全部消息；发布走类型化 Topic 的完整 wire 路径。
//
//	go test -tags integration ./...
func TestIntegrationFanOut(t *testing.T) {
	brokers := startKafka(t)

	const topic = "it.orders"
	const physical = "lynx-it-orders"
	tr, err := NewTransport(Options{Topics: map[string]TopicOptions{
		topic: {
			Brokers:  brokers,
			Topics:   []string{physical},
			Consumer: &ConsumerOptions{GroupID: "lynx-it-group", Instances: 1},
			Producer: &ProducerOptions{Topic: physical},
		},
	}})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	appCtx := lynxtest.NewContext(t)
	if err := tr.Init(appCtx); err != nil {
		t.Fatalf("transport Init: %v", err)
	}
	defer func() { _ = tr.Stop(context.Background()) }()
	trCtx, stopTr := context.WithCancel(appCtx.Context())
	defer stopTr()
	go func() { _ = tr.Start(trCtx) }()
	select {
	case <-tr.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("transport not ready")
	}

	bus := watermill.New(eventbus.Options{Transports: []eventbus.Transport{tr}})
	if err := bus.RouteKey(topic, tr, topic); err != nil {
		t.Fatalf("RouteKey: %v", err)
	}
	if err := bus.Init(appCtx); err != nil {
		t.Fatalf("bus Init: %v", err)
	}
	defer func() { _ = bus.Stop(context.Background()) }()
	busCtx, stopBus := context.WithCancel(appCtx.Context())
	defer stopBus()
	go func() { _ = bus.Start(busCtx) }()
	waitBusRunning(t, bus)

	const n = 5
	var mu sync.Mutex
	got := map[string]map[string]int{"h1": {}, "h2": {}}
	handler := func(name string) eventbus.HandlerFunc {
		return func(_ context.Context, e *eventbus.RawEvent) error {
			mu.Lock()
			got[name][e.Key]++ // WithMessageKey 写入 Key（Kafka record key）
			mu.Unlock()
			return nil
		}
	}
	if err := bus.Subscribe(busCtx, topic, handler("h1"), eventbus.WithHandlerName("h1")); err != nil {
		t.Fatalf("subscribe h1: %v", err)
	}
	if err := bus.Subscribe(busCtx, topic, handler("h2"), eventbus.WithHandlerName("h2")); err != nil {
		t.Fatalf("subscribe h2 (same event must reuse the subscription): %v", err)
	}
	// 消费组首次分配 / 首次拉取需要时间。
	time.Sleep(3 * time.Second)

	orderTopic := eventbus.NewTopic[map[string]string](topic)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("it-%d", i)
		if err := orderTopic.Publish(busCtx, map[string]string{"id": id},
			eventbus.WithBus(bus), eventbus.WithMessageKey(id)); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}

	waitUntil(t, 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got["h1"]) == n && len(got["h2"]) == n
	}, "fan-out did not deliver all messages to both handlers")
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"h1", "h2"} {
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("it-%d", i)
			if got[name][id] == 0 {
				t.Fatalf("handler %s missed key %s (fan-out broken?)", name, id)
			}
		}
	}
}

// TestIntegrationPerPartitionOrder 钉住 watermill-kafka 的每分区消费性质：
// `ConsumeClaim` 同步等待 `Acked()` 才取下一条——同一分区同时仅一条未确认
// 消息，因此同分区内确认 / 提交（MarkMessage(offset+1)）天然严格有序，
// 不存在"提交越过未确认低 offset"的窗口；`max_in_flight` 的并发只体现在
// 跨分区 / 跨物理 topic。
//
// 场景：单分区 topic、max_in_flight=10（允许全部并发），阻塞其中一条——
// 断言阻塞期间后续消息不会被投递（在途恒为 1），已提交 offset 恰好等于
// 已处理条数；释放后全部处理且提交追平。
func TestIntegrationPerPartitionOrder(t *testing.T) {
	brokers := startKafka(t)

	const topic = "it.ordered"
	const physical = "lynx-it-ordered"
	const group = "lynx-it-ordered-group"
	// 单分区 topic：验证分区内串行。
	createPartitionedTopic(t, brokers, physical, 1)

	const n = 10
	const blockKey = "it-5"
	tr, err := NewTransport(Options{
		Metrics: &MetricsOptions{Enabled: boolPtr(true), Interval: 300 * time.Millisecond},
		Topics: map[string]TopicOptions{
			topic: {
				Brokers: brokers,
				Topics:  []string{physical},
				// 关闭自动提交：每次 Ack 显式 Commit，提交时序可直接观测。
				Consumer: &ConsumerOptions{GroupID: group, Instances: 1, AutoCommitEnabled: boolPtr(false)},
				Producer: &ProducerOptions{Topic: physical},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	// lag 指标端到端：把 SDK meter provider（手动 reader）装上全局，采集器
	// 在 Start 时据此启动。
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prevMP := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMP)
		_ = mp.Shutdown(context.Background())
	})
	appCtx := lynxtest.NewContext(t)
	if err := tr.Init(appCtx); err != nil {
		t.Fatalf("transport Init: %v", err)
	}
	defer func() { _ = tr.Stop(context.Background()) }()
	trCtx, stopTr := context.WithCancel(appCtx.Context())
	defer stopTr()
	go func() { _ = tr.Start(trCtx) }()
	select {
	case <-tr.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("transport not ready")
	}

	bus := watermill.New(eventbus.Options{
		Transports: []eventbus.Transport{tr},
		// 在途上限 10：即使允许全部并发，分区内仍应串行（transport 同步确认）。
		Topics: map[string]eventbus.TopicConfig{topic: {MaxInFlight: n}},
	})
	if err := bus.RouteKey(topic, tr, topic); err != nil {
		t.Fatalf("RouteKey: %v", err)
	}
	if err := bus.Init(appCtx); err != nil {
		t.Fatalf("bus Init: %v", err)
	}
	defer func() { _ = bus.Stop(context.Background()) }()
	busCtx, stopBus := context.WithCancel(appCtx.Context())
	defer stopBus()
	go func() { _ = bus.Start(busCtx) }()
	waitBusRunning(t, bus)

	release := make(chan struct{})
	var completed atomic.Int32
	if err := bus.Subscribe(busCtx, topic, func(_ context.Context, e *eventbus.RawEvent) error {
		if e.Key == blockKey {
			<-release // 阻塞该条消息（占用分区唯一的在途位）
		}
		completed.Add(1)
		return nil
	}, eventbus.WithHandlerName("h-ordered")); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(3 * time.Second)

	orderTopic := eventbus.NewTopic[map[string]string](topic)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("it-%d", i)
		if err := orderTopic.Publish(busCtx, map[string]string{"id": id},
			eventbus.WithBus(bus), eventbus.WithMessageKey(id)); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}

	// 阻塞消息之前的若干条已完成；之后的不得被投递（分区内在途恒为 1）。
	waitUntil(t, 30*time.Second, func() bool { return completed.Load() >= 1 },
		"no message was processed")
	// 稳定窗口：处理数不再增长，且已提交 offset 恰好追平已处理数
	// （每 Ack 显式提交；提交不得越过未确认的阻塞消息）。
	waitUntil(t, 10*time.Second, func() bool {
		c := completed.Load()
		return c < n && committedOffset(t, brokers, group, physical) == int64(c)
	}, "processed count != committed offset while one message is blocked")
	stable := completed.Load()
	time.Sleep(time.Second)
	if got := completed.Load(); got != stable {
		t.Fatalf("processed count grew from %d to %d while one message is blocked; "+
			"per-partition consumption must stay serialized (in-flight = 1)", stable, got)
	}

	close(release)
	waitUntil(t, 30*time.Second, func() bool { return completed.Load() == n },
		"blocked message did not complete after release")
	waitUntil(t, 30*time.Second, func() bool { return committedOffset(t, brokers, group, physical) == n },
		"committed offset did not catch up after release")
	// lag 指标：全部提交后 partition 0 的 lag 应收敛到 0（真 broker 端到端）。
	waitUntil(t, 30*time.Second, func() bool {
		lag, ok := lagMetricValue(t, reader, physical, group)
		return ok && lag == 0
	}, "lag metric did not converge to 0")
}

// lagMetricValue 从手动 reader 采集结果中取（物理 topic, 分区 0, 消费组）的
// consumer lag；指标缺失时 ok=false。
func lagMetricValue(t *testing.T, reader *sdkmetric.ManualReader, physical, group string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != lagMetricName {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %s data type = %T", m.Name, m.Data)
			}
			for _, dp := range g.DataPoints {
				dest, _ := dp.Attributes.Value(attribute.Key("messaging.destination.name"))
				grp, _ := dp.Attributes.Value(attribute.Key("messaging.consumer.group.name"))
				part, _ := dp.Attributes.Value(attribute.Key("messaging.kafka.partition"))
				if dest.AsString() == physical && grp.AsString() == group && part.AsInt64() == 0 {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

// createPartitionedTopic 显式创建指定分区数的物理 topic（幂等）。
func createPartitionedTopic(t *testing.T, brokers []string, topic string, partitions int32) {
	t.Helper()
	admin := newClusterAdmin(t, brokers)
	defer func() { _ = admin.Close() }()
	err := admin.CreateTopic(topic, &sarama.TopicDetail{
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}, false)
	if err != nil && !errors.Is(err, sarama.ErrTopicAlreadyExists) {
		t.Fatalf("create topic %q: %v", topic, err)
	}
}

// committedOffset 读取消费组在（物理 topic, 分区 0）上已提交的 offset；
// 未提交过返回 -1。
func committedOffset(t *testing.T, brokers []string, group, topic string) int64 {
	t.Helper()
	admin := newClusterAdmin(t, brokers)
	defer func() { _ = admin.Close() }()
	resp, err := admin.ListConsumerGroupOffsets(group, map[string][]int32{topic: {0}})
	if err != nil {
		t.Fatalf("list consumer group offsets: %v", err)
	}
	block := resp.GetBlock(topic, 0)
	if block == nil {
		return -1
	}
	return block.Offset
}

func newClusterAdmin(t *testing.T, brokers []string) sarama.ClusterAdmin {
	t.Helper()
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V2_8_0_0
	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		t.Fatalf("cluster admin: %v", err)
	}
	return admin
}

// waitBusRunning 轮询总线就绪（与 watermill 测试同构；此处独立以避免
// 跨模块测试辅助耦合）。
func waitBusRunning(t *testing.T, bus eventbus.Bus) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool { return bus.CheckHealth() == nil }, "bus not running")
}

// waitUntil 轮询等待条件成立，超时即失败。
func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout after %v: %s", d, msg)
}
