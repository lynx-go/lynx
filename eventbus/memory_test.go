package eventbus

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryBusPublishSubscribe(t *testing.T) {
	b := NewMemoryBus(Options{})
	// Init with nil is allowed
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx := t.Context()
	go func() { _ = b.Start(ctx) }()

	// Wait for running
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := b.CheckHealth(); err != nil {
		t.Fatalf("bus not running: %v", err)
	}

	received := make(chan *RawEvent, 1)
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		received <- e
		return nil
	}, WithHandlerName("h1")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Give loop time to start
	time.Sleep(50 * time.Millisecond)

	if err := b.Publish(context.Background(), "order.created", map[string]string{"id": "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case e := <-received:
		if e.Topic != "order.created" {
			t.Errorf("topic = %q, want order.created", e.Topic)
		}
		if string(e.Payload) == "" {
			t.Error("payload empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive event")
	}

	// Test Typed helper
	topic := NewTopic[map[string]string]("order.typed")
	typedReceived := make(chan *Event[map[string]string], 1)
	if err := SubscribeTyped(context.Background(), b, topic, func(ctx context.Context, e *Event[map[string]string]) error {
		typedReceived <- e
		return nil
	}, WithHandlerName("h2")); err != nil {
		t.Fatalf("SubscribeTyped: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := PublishTyped(context.Background(), b, topic, map[string]string{"id": "2"}); err != nil {
		t.Fatalf("PublishTyped: %v", err)
	}
	select {
	case e := <-typedReceived:
		if e.Payload["id"] != "2" {
			t.Errorf("payload id = %q, want 2", e.Payload["id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive typed event")
	}

	// Dynamic subscribe after Start
	var count atomic.Int32
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		count.Add(1)
		return nil
	}, WithHandlerName("h3")); err != nil {
		t.Fatalf("dynamic Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	_ = b.Publish(context.Background(), "order.created", []byte("hello"))
	time.Sleep(100 * time.Millisecond)
	if count.Load() == 0 {
		t.Error("dynamic subscriber did not receive")
	}

	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSubscribeHandlerNameDefaultsToTopic(t *testing.T) {
	b := NewMemoryBus(Options{})
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Start(ctx) }()
	for i := 0; i < 50; i++ {
		if b.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := make(chan string, 1)
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		got <- e.Topic
		return nil
	}); err != nil {
		t.Fatalf("Subscribe without handler name: %v", err)
	}

	// 同 topic 再订一次且不指定 handlerName → 默认同名，应冲突
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		return nil
	}); err == nil {
		t.Fatal("expected duplicate handler name error")
	}

	// 显式命名可并存
	if err := b.Subscribe(context.Background(), "order.created", func(ctx context.Context, e *RawEvent) error {
		return nil
	}, WithHandlerName("audit")); err != nil {
		t.Fatalf("Subscribe WithHandlerName: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	_ = b.Publish(context.Background(), "order.created", map[string]string{"id": "1"})
	select {
	case topic := <-got:
		if topic != "order.created" {
			t.Fatalf("topic = %q", topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive")
	}
	_ = b.Stop(context.Background())
}

func TestMemoryBusWithAppContext(t *testing.T) {
	b := NewMemoryBus(Options{BufferSize: 10})
	// Test Bus via lynx App
	// Use helper to ensure Bus works with Init
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init nil: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Start(ctx)
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{}, 1)
	_ = b.Subscribe(context.Background(), "test", func(ctx context.Context, e *RawEvent) error {
		done <- struct{}{}
		return nil
	})
	time.Sleep(20 * time.Millisecond)
	_ = b.Publish(context.Background(), "test", "payload")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscribe failed")
	}
	_ = b.Stop(context.Background())
}

// TestMemoryBusConcurrentPublishStopNoPanic 是 CORE-01 的回归：dispatch
// 的发送必须与 Stop 的写锁互斥。修复前 dispatch 在 RLock 内只拷贝订阅者、
// 解锁后发送，与 Stop 的 close(sub.ch) 竞态会 send on closed channel panic。
// 多轮并发 Publish+Stop 在 -race 下通过且不 panic 即为修复生效。
func TestMemoryBusConcurrentPublishStopNoPanic(t *testing.T) {
	const rounds = 100
	for i := 0; i < rounds; i++ {
		b := NewMemoryBus(Options{})
		_ = b.Init(nil)
		runCtx, cancelRun := context.WithCancel(context.Background())
		go func() { _ = b.Start(runCtx) }()
		waitRunning(t, b)

		if err := b.Subscribe(context.Background(), "race.topic", func(context.Context, *RawEvent) error {
			return nil
		}, WithHandlerName("h-race")); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		stopDone := make(chan struct{})
		go func() {
			defer close(stopDone)
			_ = b.Stop(context.Background())
		}()
		// 并发发布直到 Stop 胜出（Publish 报 bus is closed）：发布压力
		// 需覆盖"已过 closed 检查、正要 dispatch"的竞态窗口。
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					if err := b.Publish(context.Background(), "race.topic", "p"); err != nil {
						return
					}
				}
			}()
		}
		<-stopDone
		wg.Wait()
		cancelRun()
	}
}

// fakeInitContext 以固定 logger 初始化 Bus（memoryBus.Init 仅取 Logger）。
type fakeInitContext struct{ logger *slog.Logger }

func (c *fakeInitContext) Context() context.Context        { return context.Background() }
func (c *fakeInitContext) Logger(args ...any) *slog.Logger { return c.logger }

// TestMemoryBusDropLogCarriesEventID 是 CORE-10 的回归：缓冲满丢弃事件的
// Error 日志必须带事件 ID——内存 Bus 是 at-most-once 语义，丢失时没有
// ID 就无法对账丢的是哪条。
func TestMemoryBusDropLogCarriesEventID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := NewMemoryBus(Options{BufferSize: 1})
	if err := bus.Init(&fakeInitContext{logger: logger}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	release := make(chan struct{})
	if err := bus.Subscribe(context.Background(), "drop.topic", func(context.Context, *RawEvent) error {
		<-release
		return nil
	}, WithHandlerName("h-drop")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// 第一条被阻塞的 handler 持有，第二条填满缓冲（BufferSize=1），
	// 第三条起必然触发丢弃路径。
	for i := 0; i < 5; i++ {
		if err := bus.Publish(context.Background(), "drop.topic", []byte("p")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	close(release)
	_ = bus.Stop(context.Background())

	out := buf.String()
	if !strings.Contains(out, "bus dispatch dropped event") {
		t.Fatalf("drop log missing, got: %q", out)
	}
	if !strings.Contains(out, " id=") {
		t.Errorf("drop log must carry the event id, got: %q", out)
	}
}
