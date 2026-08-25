package watermill_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

func okRawHandler(context.Context, *eventbus.RawEvent) error { return nil }

// TestSubscribeNonMemorySharedGroupRejected 回归 WK-01：Kafka 等消费组后端上，
// 同一 topic 被两个不同 handler 以同一消费组订阅会静默瓜分分区（各收一半
// 消息）。Bus 必须在订阅期拒绝：默认组（未配置 group）与显式同组均报错，
// 错误信息需指出冲突双方与 WithGroup 逃生口。
func TestSubscribeNonMemorySharedGroupRejected(t *testing.T) {
	rt := &recordingTransport{topic: "order.created"}
	bus := watermill.New(eventbus.Options{Transports: []eventbus.Transport{rt}})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx := context.Background()

	// 默认组占用：第二个 handler 复用（空）默认组被拒。
	if err := bus.Subscribe(ctx, "order.created", okRawHandler, eventbus.WithHandlerName("h1")); err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	err := bus.Subscribe(ctx, "order.created", okRawHandler, eventbus.WithHandlerName("h2"))
	if err == nil {
		t.Fatal("want error: two handlers sharing the default group on non-memory transport")
	}
	for _, want := range []string{"order.created", "h1", "WithGroup"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err.Error(), want)
		}
	}

	// 显式同组同样被拒。
	if err := bus.Subscribe(ctx, "order.created", okRawHandler, eventbus.WithHandlerName("g1"), eventbus.WithGroup("payments")); err != nil {
		t.Fatalf("subscribe with explicit group: %v", err)
	}
	if err := bus.Subscribe(ctx, "order.created", okRawHandler, eventbus.WithHandlerName("g2"), eventbus.WithGroup("payments")); err == nil {
		t.Fatal("want error: two handlers sharing explicit group payments")
	}

	// 不同组共存（广播语义的逃生口）。
	if err := bus.Subscribe(ctx, "order.created", okRawHandler, eventbus.WithHandlerName("audit"), eventbus.WithGroup("audit")); err != nil {
		t.Fatalf("subscribe with distinct group must be allowed: %v", err)
	}

	// 单 handler 多实例（竞争消费）不受影响。
	if err := bus.Subscribe(ctx, "order.created", okRawHandler, eventbus.WithHandlerName("workers"), eventbus.WithGroup("workers"), eventbus.WithInstances(3)); err != nil {
		t.Fatalf("single handler with instances must be allowed: %v", err)
	}
}

// TestSubscribeMemoryTransportBroadcastAllowed 回归 WK-01 正向面：
// 内存 Transport 是广播语义，同 topic 多 handler 不受消费组占用限制。
func TestSubscribeMemoryTransportBroadcastAllowed(t *testing.T) {
	mem := watermill.NewMemoryTransport()
	bus := watermill.New(eventbus.Options{DefaultTransport: mem})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	got1 := make(chan string, 1)
	got2 := make(chan string, 1)
	h1 := func(ctx context.Context, e *eventbus.RawEvent) error {
		got1 <- e.ID
		return nil
	}
	h2 := func(ctx context.Context, e *eventbus.RawEvent) error {
		got2 <- e.ID
		return nil
	}
	if err := bus.Subscribe(ctx, "order.created", h1, eventbus.WithHandlerName("b1")); err != nil {
		t.Fatalf("subscribe b1: %v", err)
	}
	if err := bus.Subscribe(ctx, "order.created", h2, eventbus.WithHandlerName("b2")); err != nil {
		t.Fatalf("subscribe b2 (memory transport must allow broadcast): %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := bus.Publish(ctx, "order.created", map[string]string{"id": "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for name, ch := range map[string]chan string{"b1": got1, "b2": got2} {
		select {
		case <-ch: // 广播：两个 handler 都要收到
		case <-time.After(3 * time.Second):
			t.Fatalf("handler %s did not receive broadcast event", name)
		}
	}
	stopWithin(t, bus, 2*time.Second)
}

// redeliveringTransport 模拟 Kafka 的 Nack 重投语义（ResendLoop）：
// Nack 后把同一事件重新投给订阅者，形成无限重投循环，用于验证 Bus 级
// 重投上限（WK-02）。
type redeliveringTransport struct {
	topic string
	mu    sync.Mutex
	subs  []chan eventbus.Delivery
	acked chan struct{}
}

func (t *redeliveringTransport) Publish(ctx context.Context, topic string, e *eventbus.RawEvent) error {
	t.deliver(e)
	return nil
}

func (t *redeliveringTransport) deliver(e *eventbus.RawEvent) {
	t.mu.Lock()
	subs := append([]chan eventbus.Delivery(nil), t.subs...)
	t.mu.Unlock()
	for _, ch := range subs {
		go func(ch chan eventbus.Delivery) {
			ch <- eventbus.Delivery{
				Event: e,
				Ack: func() {
					select {
					case t.acked <- struct{}{}:
					default:
					}
				},
				Nack: func() { t.deliver(e) },
			}
		}(ch)
	}
}

func (t *redeliveringTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan eventbus.Delivery, error) {
	ch := make(chan eventbus.Delivery, 8)
	t.mu.Lock()
	t.subs = append(t.subs, ch)
	t.mu.Unlock()
	go func() {
		<-ctx.Done()
		t.mu.Lock()
		for i, s := range t.subs {
			if s == ch {
				t.subs = append(t.subs[:i], t.subs[i+1:]...)
				break
			}
		}
		t.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

func (t *redeliveringTransport) Topics() []string { return []string{t.topic} }
func (t *redeliveringTransport) Close() error     { return nil }

// TestMaxRedeliveriesDropsPoisonMessage 回归 WK-02：handler 恒失败 + Transport
// 无限重投时，Bus 必须在 MaxRedeliveries 轮终态失败后 Ack 丢弃毒消息；
// 上限为 3 意味着 1 次初始投递 + 3 次重投后放弃。
func TestMaxRedeliveriesDropsPoisonMessage(t *testing.T) {
	rt := &redeliveringTransport{topic: "order.poison", acked: make(chan struct{}, 1)}
	bus := watermill.New(
		eventbus.Options{
			Transports: []eventbus.Transport{rt},
			// 关闭内层重试：一轮投递 = 一次终态失败，投递轮数可直接断言。
			Retry: &eventbus.RetryOptions{MaxRetries: 0},
		},
		watermill.WithMaxRedeliveries(3),
	)
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	var attempts atomic.Int32
	if err := bus.Subscribe(ctx, "order.poison", func(ctx context.Context, e *eventbus.RawEvent) error {
		attempts.Add(1)
		return errors.New("poison")
	}, eventbus.WithHandlerName("poison-h")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := bus.Publish(ctx, "order.poison", map[string]string{"id": "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// 超过上限后消息被 Ack 丢弃（不再重投）。
	select {
	case <-rt.acked:
	case <-time.After(3 * time.Second):
		t.Fatalf("poison message was never dropped (attempts=%d)", attempts.Load())
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("attempts = %d, want 4 (1 delivery + 3 redeliveries)", got)
	}
	// 丢弃后循环终止：不再有新的投递。
	time.Sleep(200 * time.Millisecond)
	if got := attempts.Load(); got != 4 {
		t.Fatalf("attempts grew to %d after drop; redelivery loop not stopped", got)
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestMaxRedeliveriesCounterClearedOnSuccess 验证 WK-02 的计数清除语义：
// 消息处理成功后计数归零，同 ID 的后续投递不会被陈旧计数误杀。
// handler 奇数次投递失败、偶数次成功；上限 1 下若成功不清除计数，
// 第二轮的首个失败（累计 2 > 1）会把消息直接丢弃，attempts 停在 3。
func TestMaxRedeliveriesCounterClearedOnSuccess(t *testing.T) {
	rt := &redeliveringTransport{topic: "order.retry", acked: make(chan struct{}, 1)}
	bus := watermill.New(
		eventbus.Options{
			Transports: []eventbus.Transport{rt},
			Retry:      &eventbus.RetryOptions{MaxRetries: 0},
		},
		watermill.WithMaxRedeliveries(1),
	)
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	var attempts atomic.Int32
	if err := bus.Subscribe(ctx, "order.retry", func(ctx context.Context, ev *eventbus.RawEvent) error {
		if attempts.Add(1)%2 == 1 {
			return errors.New("transient") // 奇数次失败 → Nack 重投
		}
		return nil // 偶数次成功 → Ack
	}, eventbus.WithHandlerName("flaky-h")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// 同一 ID 投递两轮：每轮都是"失败一次、重投后成功"。
	e := &eventbus.RawEvent{ID: "same-id", Payload: []byte("x"), Headers: map[string]string{}}
	for round := 1; round <= 2; round++ {
		if err := rt.Publish(ctx, "order.retry", e); err != nil {
			t.Fatalf("Publish round %d: %v", round, err)
		}
		deadline := time.Now().Add(3 * time.Second)
		want := int32(round * 2)
		for time.Now().Before(deadline) {
			if attempts.Load() >= want {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got := attempts.Load(); got < want {
			t.Fatalf("round %d: attempts = %d, want >= %d (stale counter dropped the message)", round, got, want)
		}
		select {
		case <-rt.acked:
		default:
			t.Fatalf("round %d: message not acknowledged", round)
		}
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestMaxRedeliveriesIndependentPerHandler 回归复审-4：同一 Kafka 消息投给
// 两个不同消费组（两个 handler 各收一份）时，重投计数必须按 handler 隔离
// ——成功 handler 的 success 不得清零失败 handler 的累计计数，否则毒消息
// 永不达上限。h-ok 恒成功、h-fail 恒失败：失败侧独立累计至上限（2）后
// 丢弃，重投循环终止；成功侧每轮正常 Ack，不受影响。
func TestMaxRedeliveriesIndependentPerHandler(t *testing.T) {
	rt := &redeliveringTransport{topic: "order.fanout", acked: make(chan struct{}, 1)}
	bus := watermill.New(
		eventbus.Options{
			Transports: []eventbus.Transport{rt},
			Retry:      &eventbus.RetryOptions{MaxRetries: 0},
		},
		watermill.WithMaxRedeliveries(2),
	)
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	var okAttempts, failAttempts atomic.Int32
	if err := bus.Subscribe(ctx, "order.fanout", func(ctx context.Context, e *eventbus.RawEvent) error {
		okAttempts.Add(1)
		return nil
	}, eventbus.WithHandlerName("h-ok"), eventbus.WithGroup("g-ok")); err != nil {
		t.Fatalf("Subscribe h-ok: %v", err)
	}
	if err := bus.Subscribe(ctx, "order.fanout", func(ctx context.Context, e *eventbus.RawEvent) error {
		failAttempts.Add(1)
		return errors.New("poison")
	}, eventbus.WithHandlerName("h-fail"), eventbus.WithGroup("g-fail")); err != nil {
		t.Fatalf("Subscribe h-fail: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// 同一 ID 的消息广播给两个 handler：h-ok 每轮成功（若计数被共享，
	// 它的 success 会清零 h-fail 的累计），h-fail 每轮终态失败。
	e := &eventbus.RawEvent{ID: "shared-id", Payload: []byte("x"), Headers: map[string]string{}}
	if err := rt.Publish(ctx, "order.fanout", e); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// 失败侧在上限（1 次投递 + 2 次重投）后丢弃。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && failAttempts.Load() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := failAttempts.Load(); got != 3 {
		t.Fatalf("fail handler attempts = %d, want 3 (success side cleared the poison counter?)", got)
	}
	if got := okAttempts.Load(); got < 3 {
		t.Fatalf("ok handler attempts = %d, want >= 3 (must receive every round)", got)
	}
	// 丢弃后循环终止：两侧都不再有新投递。
	time.Sleep(200 * time.Millisecond)
	if got := failAttempts.Load(); got != 3 {
		t.Fatalf("fail attempts grew to %d after drop; redelivery loop not stopped", got)
	}
	if got := okAttempts.Load(); got != 3 {
		t.Fatalf("ok attempts = %d, want 3 (loop must stop once failing side drops)", got)
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestMemoryTransportCheckHealthLifecycle 回归 WK-04：CheckHealth 此前无任何
// 置位路径恒报错；现在首次 Publish/Subscribe 视为运行，Close 复位。
func TestMemoryTransportCheckHealthLifecycle(t *testing.T) {
	mem := watermill.NewMemoryTransport()
	if err := mem.CheckHealth(); err == nil {
		t.Fatal("want unhealthy before first use")
	}
	if err := mem.Publish(context.Background(), "t", &eventbus.RawEvent{ID: "1", Headers: map[string]string{}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := mem.CheckHealth(); err != nil {
		t.Fatalf("want healthy after first use, got: %v", err)
	}
	if err := mem.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := mem.CheckHealth(); err == nil {
		t.Fatal("want unhealthy after Close")
	}
}

// TestStartFailureRollsBackStarted 回归 WK-11：Start 失败（router.Run 在订阅
// 阶段失败）必须回滚 started，后续订阅走 pending 路径成功返回，而不是
// 撞上"router 未运行"的 RunHandlers 错误；健康检查不得谎报运行中。
func TestStartFailureRollsBackStarted(t *testing.T) {
	fake := &nonMemoryTransport{topics: []string{"order.created"}} // Subscribe 恒失败
	bus := watermill.New(eventbus.Options{
		Transports:       []eventbus.Transport{fake},
		DefaultTransport: watermill.NewMemoryTransport(),
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bus.Subscribe(context.Background(), "order.created", okRawHandler, eventbus.WithHandlerName("h1")); err != nil {
		t.Fatalf("pending subscribe: %v", err)
	}
	startErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { startErr <- bus.Start(ctx) }()
	select {
	case err := <-startErr:
		if err == nil {
			t.Fatal("want Start error (subscriber Subscribe fails)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return")
	}
	// started 已回滚：新订阅进入 pending 并成功（而非 RunHandlers 报错）。
	if err := bus.Subscribe(context.Background(), "other.topic", okRawHandler, eventbus.WithHandlerName("h2")); err != nil {
		t.Fatalf("subscribe after failed Start should go pending, got: %v", err)
	}
	if err := bus.CheckHealth(); err == nil {
		t.Fatal("bus must not report healthy after failed Start")
	}
}
