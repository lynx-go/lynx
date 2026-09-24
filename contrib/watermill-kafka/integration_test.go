//go:build integration

package kafka

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/lynxtest"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
)

// TestIntegrationFanOut 对真实 Kafka（testcontainers）验证 v1.16 消费模型：
// 同一事件的两个 handler 共享一条 transport 订阅（消费组 / 成员数只来自
// transport 配置）并各自收到全部消息；发布走类型化 Topic 的完整 wire 路径。
//
//	go test -tags integration ./...
//
// Docker / 镜像不可用时跳过（容器启动失败即视为环境缺失）。
func TestIntegrationFanOut(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

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

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(got["h1"]) == n && len(got["h2"]) == n
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"h1", "h2"} {
		if len(got[name]) != n {
			t.Fatalf("handler %s received %d/%d distinct messages: %v", name, len(got[name]), n, got[name])
		}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("it-%d", i)
			if got[name][id] == 0 {
				t.Fatalf("handler %s missed key %s (fan-out broken?)", name, id)
			}
		}
	}
}

// waitBusRunning 轮询总线就绪（与 watermill 测试同构；此处独立以避免
// 跨模块测试辅助耦合）。
func waitBusRunning(t *testing.T, bus eventbus.Bus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if bus.CheckHealth() == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("bus not running")
}
