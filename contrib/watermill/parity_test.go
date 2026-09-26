package watermill_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

// TestDeliverySemanticsParity 把同一场景矩阵分别在内存 Bus 与
// Watermill(+MemoryTransport) 上执行，断言 handler 调用次数一致——
// AutoAck / ContinueOnError / 重试预算的处置语义跨实现相同
// （at-most-once vs at-least-once 差异只体现在失败终态之后：内存丢弃、
// 持久化重投，本测试用 MemoryTransport 不触发重投）。
// 接口是测试面：语义承诺钉在两种实现的公共行为上，而非各自的注释里。
func TestDeliverySemanticsParity(t *testing.T) {
	// 重试预算统一 2 次、间隔 1ms，双实现一致且测试快速。
	retry := eventbus.RetryOptions{MaxRetries: 2, Backoff: time.Millisecond}

	cases := []struct {
		name string
		opts []eventbus.SubscribeOption
		// failTimes: handler 前 N 次失败。
		failTimes int
		// wantCalls: 两种实现一致的调用次数（AutoAck/ContinueOnError 抑制
		// 重试、预算内重试后成功——处置语义跨实现相同）。
		wantCalls int
		// divergesOnExhaustion: 重试耗尽是文档声明的有意分歧点（Bus 接口
		// 注释）——内存 Bus at-most-once 丢弃（wantCalls 次）；持久化语义
		// （含 gochannel）at-least-once：Nack 重投，每轮重投重新执行预算内
		// 重试，直至重投上限。此场景断言分歧本身而非一致。
		divergesOnExhaustion bool
	}{
		{
			name:      "retry-then-success",
			opts:      []eventbus.SubscribeOption{eventbus.WithSubscribeRetry(retry)},
			failTimes: 2,
			wantCalls: 3,
		},
		{
			name:                 "retry-exhausted-memory-drops-watermill-redelivers",
			opts:                 []eventbus.SubscribeOption{eventbus.WithSubscribeRetry(retry)},
			failTimes:            99,
			wantCalls:            3, // 内存侧期望值；watermill 侧断言 ≥ 两轮重投
			divergesOnExhaustion: true,
		},
		{
			name:      "autoack-suppresses-retry",
			opts:      []eventbus.SubscribeOption{eventbus.WithSubscribeRetry(retry), eventbus.WithAutoAck()},
			failTimes: 99,
			wantCalls: 1,
		},
		{
			name:      "continue-on-error-suppresses-retry",
			opts:      []eventbus.SubscribeOption{eventbus.WithSubscribeRetry(retry), eventbus.WithContinueOnError()},
			failTimes: 99,
			wantCalls: 1,
		},
	}

	newBuses := map[string]func(t *testing.T) eventbus.Bus{
		"memory": func(t *testing.T) eventbus.Bus {
			bus := eventbus.NewMemoryBus(eventbus.Options{})
			startParityBus(t, bus)
			return bus
		},
		"watermill": func(t *testing.T) eventbus.Bus {
			bus := watermill.New(eventbus.Options{DefaultTransport: watermill.NewMemoryTransport()})
			startParityBus(t, bus)
			return bus
		},
	}

	for impl, makeBus := range newBuses {
		for _, tc := range cases {
			t.Run(impl+"/"+tc.name, func(t *testing.T) {
				bus := makeBus(t)
				topic := "parity." + tc.name

				var calls atomic.Int32
				first := make(chan struct{})
				done := make(chan struct{}) // 成功终态（failTimes 次失败后）
				var doneOnce bool
				if err := bus.Subscribe(context.Background(), topic, func(ctx context.Context, e *eventbus.RawEvent) error {
					n := int(calls.Add(1))
					if n == 1 {
						close(first)
					}
					if n > tc.failTimes {
						if !doneOnce {
							doneOnce = true
							close(done)
						}
						return nil
					}
					return errors.New("parity failure")
				}, append([]eventbus.SubscribeOption{eventbus.WithHandlerName(tc.name)}, tc.opts...)...); err != nil {
					t.Fatalf("Subscribe: %v", err)
				}

				if err := bus.Publish(context.Background(), topic, map[string]string{"k": "v"}); err != nil {
					t.Fatalf("Publish: %v", err)
				}

				// 成功场景等终态；纯失败场景等首调后静默窗口（重试间隔 1ms，
				// 若语义被破坏，预期外的调用会在毫秒级出现，250ms 余量足够）。
				if tc.failTimes <= 3 {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						// 落到下方计数断言报具体值
					}
				} else {
					select {
					case <-first:
					case <-time.After(3 * time.Second):
						t.Fatal("handler never invoked")
					}
					time.Sleep(250 * time.Millisecond)
				}

				got := int(calls.Load())
				if tc.divergesOnExhaustion && impl == "watermill" {
					// at-least-once：至少完成一轮完整重投（2 次重试 + 首调的
					// 下一轮）才证明 Nack 路径真实生效；上限由重投预算约束
					//（默认 10 轮），不在此钉死具体值。
					if want := 2 * (tc.wantCalls + 1); got < want {
						t.Fatalf("%s: handler calls = %d, want >= %d (redelivery rounds must run)", impl, got, want)
					}
					return
				}
				if got != tc.wantCalls {
					t.Fatalf("%s: handler calls = %d, want %d", impl, got, tc.wantCalls)
				}
			})
		}
	}
}

// startParityBus 初始化并启动总线，注册清理（等待运行 + 停止限时）。
func startParityBus(t *testing.T, bus eventbus.Bus) {
	t.Helper()
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)
	t.Cleanup(func() { stopWithin(t, bus, 2*time.Second) })
}

// parityImpls 是矩阵覆盖的两个进程内实现（memory 与 watermill+MemoryTransport）。
var parityImpls = []string{"memory", "watermill"}

// newParityBus 按实现名构造并启动一个 parity Bus。
func newParityBus(t *testing.T, impl string) eventbus.Bus {
	t.Helper()
	var bus eventbus.Bus
	switch impl {
	case "memory":
		bus = eventbus.NewMemoryBus(eventbus.Options{})
	case "watermill":
		bus = watermill.New(eventbus.Options{DefaultTransport: watermill.NewMemoryTransport()})
	default:
		t.Fatalf("unknown impl %q", impl)
	}
	startParityBus(t, bus)
	return bus
}

// TestFanOutParity：同一事件的两个 handler 都收到每条消息（进程内扇出），
// 且各恰一次——两个实现语义一致。
func TestFanOutParity(t *testing.T) {
	for _, impl := range parityImpls {
		t.Run(impl, func(t *testing.T) {
			bus := newParityBus(t, impl)
			topic := "parity.fanout"
			var first, second atomic.Int32
			subscribe := func(name string, counter *atomic.Int32) {
				t.Helper()
				if err := bus.Subscribe(context.Background(), topic, func(context.Context, *eventbus.RawEvent) error {
					counter.Add(1)
					return nil
				}, eventbus.WithHandlerName(name)); err != nil {
					t.Fatalf("Subscribe(%s): %v", name, err)
				}
			}
			subscribe("h1", &first)
			subscribe("h2", &second)
			if err := bus.Publish(context.Background(), topic, map[string]string{"k": "v"}); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			waitCond(t, 3*time.Second, func() bool {
				return first.Load() == 1 && second.Load() == 1
			}, "both handlers must receive the event")
			time.Sleep(100 * time.Millisecond)
			if got := first.Load(); got != 1 {
				t.Fatalf("first handler calls = %d, want 1", got)
			}
			if got := second.Load(); got != 1 {
				t.Fatalf("second handler calls = %d, want 1", got)
			}
		})
	}
}

// TestHandlerTimeoutParity：handler 单次尝试超时按失败处理（重试预算内重试），
// 且两实现的终态分歧显式断言——内存 Bus at-most-once：预算耗尽即丢弃
// （调用次数精确为预算内尝试数）；watermill at-least-once：Nack 重投，由
// 毒消息止损上界收口（调用次数增长到有界后停止，不是无限重投）。
func TestHandlerTimeoutParity(t *testing.T) {
	const perCall = 50 * time.Millisecond
	retry := eventbus.RetryOptions{MaxRetries: 1, Backoff: time.Millisecond}

	for _, impl := range parityImpls {
		t.Run(impl, func(t *testing.T) {
			bus := newParityBus(t, impl)
			topic := "parity.timeout"
			tp := eventbus.NewTopic[map[string]string](topic, eventbus.WithTopicHandlerTimeout(perCall))

			var calls atomic.Int32
			release := make(chan struct{})
			defer close(release)
			handler := func(ctx context.Context, _ *eventbus.Event[map[string]string]) error {
				calls.Add(1)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if err := tp.Subscribe(context.Background(), handler,
				eventbus.WithBus(bus),
				eventbus.WithHandlerName("h-timeout"),
				eventbus.WithSubscribeRetry(retry)); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			if err := tp.Publish(context.Background(), map[string]string{"k": "v"}, eventbus.WithBus(bus)); err != nil {
				t.Fatalf("Publish: %v", err)
			}

			if impl == "memory" {
				// at-most-once：重试预算耗尽即丢弃（1 次首调 + 1 次重试）。
				waitCond(t, 3*time.Second, func() bool { return calls.Load() == 2 },
					"memory bus must attempt exactly budget+1 times")
				time.Sleep(200 * time.Millisecond)
				if got := calls.Load(); got != 2 {
					t.Fatalf("memory calls = %d, want exactly 2 (drop after retries)", got)
				}
				return
			}
			// at-least-once：超时 → Nack → 重投（调用次数 > 预算内尝试数）。
			waitCond(t, 3*time.Second, func() bool { return calls.Load() >= 4 },
				"watermill must redeliver after retry exhaustion")
			// 毒消息止损上界：计数最终停止增长（默认 10 轮重投内收口；
			// gochannel 重投有间隔，用自适应稳定窗口而非固定 sleep）。
			stable := calls.Load()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				time.Sleep(300 * time.Millisecond)
				got := calls.Load()
				if got == stable {
					break
				}
				stable = got
			}
			time.Sleep(300 * time.Millisecond)
			if got := calls.Load(); got != stable {
				t.Fatalf("watermill calls kept growing: %d -> %d (redelivery stop-loss must bound it)", stable, got)
			}
			if stable > 40 {
				t.Fatalf("watermill calls = %d, want bounded by redelivery limit", stable)
			}
		})
	}
}

// TestStopSemanticsParity：Stop 幂等；停止后 Publish/Subscribe 一律拒绝。
func TestStopSemanticsParity(t *testing.T) {
	for _, impl := range parityImpls {
		t.Run(impl, func(t *testing.T) {
			bus := newParityBus(t, impl)
			if err := bus.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if err := bus.Stop(context.Background()); err != nil {
				t.Fatalf("second Stop: %v", err)
			}
			if err := bus.Publish(context.Background(), "parity.stopped", map[string]string{}); err == nil {
				t.Error("Publish after Stop = nil, want error")
			}
			if err := bus.Subscribe(context.Background(), "parity.stopped",
				func(context.Context, *eventbus.RawEvent) error { return nil },
				eventbus.WithHandlerName("h")); err == nil {
				t.Error("Subscribe after Stop = nil, want error")
			}
		})
	}
}

// TestMemoryBufferFullDrops：有意分歧项（仅内存侧断言）——订阅者缓冲满时
// 丢弃（at-most-once，见 design-eventbus.md §5.6 对照表）；watermill 路径
// 无对应语义（Nack 重投由止损上界收口，见 TestHandlerTimeoutParity）。
func TestMemoryBufferFullDrops(t *testing.T) {
	bus := eventbus.NewMemoryBus(eventbus.Options{BufferSize: 1})
	startParityBus(t, bus)
	topic := "parity.drop"

	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	if err := bus.Subscribe(context.Background(), topic, func(context.Context, *eventbus.RawEvent) error {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
		}
		<-release
		return nil
	}, eventbus.WithHandlerName("h-drop")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := bus.Publish(context.Background(), topic, map[string]string{"n": "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	<-entered // 首条已进入 handler（占住处理位）
	// 第二条进缓冲（容量 1），第三条缓冲满被丢弃。
	for i := 2; i <= 3; i++ {
		if err := bus.Publish(context.Background(), topic, map[string]string{"n": "x"}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(release)

	waitCond(t, 2*time.Second, func() bool { return calls.Load() == 2 },
		"buffered message must be delivered after release")
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 (third message dropped on full buffer)", got)
	}
}
