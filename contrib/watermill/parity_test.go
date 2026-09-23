package watermill_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/contrib/watermill"
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
