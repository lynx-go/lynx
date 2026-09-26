package watermill

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// forceStarted 直接构造 Start 的竞态窗口（WK-03 场景）：started=true 但
// router.Run 尚未执行，用于确定性地测试动态订阅路径。
func forceStarted(b *Bus) {
	b.mu.Lock()
	b.started = true
	b.runCtx = context.Background()
	b.mu.Unlock()
}

// TestDynamicSubscribeOnNonRunningRouterKeepsName 回归 WK-03：
// 竞态窗口内动态订阅 addHandler 成功但 RunHandlers 失败。修复后必须：
// 1) 返回明确错误；2) 不回滚 handlerNames——同名重试得到 duplicate 错误
// （而非撞上 router 内残留的幽灵 handler 触发 watermill panic）。
func TestDynamicSubscribeOnNonRunningRouterKeepsName(t *testing.T) {
	b := New(eventbus.Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	forceStarted(b)

	err := b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h1"))
	if err == nil {
		t.Fatal("want error: RunHandlers on non-running router")
	}
	if !strings.Contains(err.Error(), "router not running") {
		t.Fatalf("error should explain router is not running, got: %v", err)
	}
	// 复审-2：文案必须警告勿换名重订阅（换名会导致双消费或 WK-01 冲突）。
	if !strings.Contains(err.Error(), "do not resubscribe under a different handler name") {
		t.Fatalf("error should warn against resubscribing with a different name, got: %v", err)
	}
	// 不回滚：handlerNames 保持占用。
	b.mu.Lock()
	_, taken := b.handlerNames["h1"]
	b.mu.Unlock()
	if !taken {
		t.Fatal("handlerNames must keep the name occupied after RunHandlers failure")
	}
	// 同名重试得到 duplicate 错误而非 panic。
	err = b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h1"))
	if err == nil || !strings.Contains(err.Error(), "duplicate handler name") {
		t.Fatalf("retry err = %v, want duplicate handler name", err)
	}
}

// TestDynamicSubscribeDuplicateHandlerPanicTranslated 回归 WK-03：
// router 内残留幽灵订阅（订阅注册表不知情）时，动态订阅的
// AddConsumerHandler 会 panic（DuplicateHandlerNameError）——必须被翻译为
// 错误返回，不得击穿进程；且该 handler 名保持占用（重试得到 duplicate）。
func TestDynamicSubscribeDuplicateHandlerPanicTranslated(t *testing.T) {
	b := New(eventbus.Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	forceStarted(b)
	// 制造幽灵订阅：直接进 router，绕过订阅注册表。router 名与订阅名
	// （sub:<逻辑 topic>）一致。
	ghost := "sub:order.created"
	b.router.AddConsumerHandler(ghost, "order.created", &subscriberAdapter{}, func(*message.Message) error { return nil })

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Subscribe panicked on duplicate handler name: %v", r)
		}
	}()
	err := b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h1"))
	if err == nil || !strings.Contains(err.Error(), ghost) {
		t.Fatalf("err = %v, want translated duplicate-subscription error mentioning %q", err, ghost)
	}
	if !errors.Is(err, errHandlerNameTaken) {
		t.Fatalf("err = %v, want errHandlerNameTaken in chain", err)
	}
	err = b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h1"))
	if err == nil || !strings.Contains(err.Error(), "duplicate handler name") {
		t.Fatalf("retry err = %v, want duplicate handler name", err)
	}
}

// TestMaxRedeliveriesFor 验证上限解析优先级：主题级非零 > Bus 级；
// 负数（Bus 级或主题级）禁用；缺省 = DefaultMaxRedeliveries。
func TestMaxRedeliveriesFor(t *testing.T) {
	if limit, ok := New(eventbus.Options{}).maxRedeliveriesFor("x"); !ok || limit != DefaultMaxRedeliveries {
		t.Fatalf("default: limit=%d ok=%v", limit, ok)
	}
	b := New(eventbus.Options{}, WithMaxRedeliveries(5))
	if limit, ok := b.maxRedeliveriesFor("x"); !ok || limit != 5 {
		t.Fatalf("bus-level: limit=%d ok=%v", limit, ok)
	}
	bt := New(eventbus.Options{}, WithMaxRedeliveries(5), WithTopicMaxRedeliveries("t", 2))
	if limit, ok := bt.maxRedeliveriesFor("t"); !ok || limit != 2 {
		t.Fatalf("topic override: limit=%d ok=%v", limit, ok)
	}
	if limit, ok := bt.maxRedeliveriesFor("other"); !ok || limit != 5 {
		t.Fatalf("unoverridden topic: limit=%d ok=%v", limit, ok)
	}
	if _, ok := New(eventbus.Options{}, WithMaxRedeliveries(-1)).maxRedeliveriesFor("x"); ok {
		t.Fatal("negative bus-level must disable the limit")
	}
	if _, ok := New(eventbus.Options{}, WithMaxRedeliveries(5), WithTopicMaxRedeliveries("t", -1)).maxRedeliveriesFor("t"); ok {
		t.Fatal("negative topic-level must disable the limit")
	}
}

// TestAttachHandlerMaxInFlightFirstWins 钉住订阅级 MaxInFlight 的首值语义
// （事件级配置）：同名事件第二个 handler 解析出不同值时首值生效并记 Warn；
// 值相等不告警（避免修复前的误报）。
func TestAttachHandlerMaxInFlightFirstWins(t *testing.T) {
	c := &captureLogs{level: slog.LevelWarn}
	b := New(eventbus.Options{DefaultTransport: NewMemoryTransport()})
	b.logger = slog.New(c)
	if err := b.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	noop := func(context.Context, *eventbus.RawEvent) error { return nil }
	for _, h := range []eventbus.ResolvedSubscription{
		{HandlerName: "h1", MaxInFlight: 2},
		{HandlerName: "h2", MaxInFlight: 3}, // 不一致：首值 2 生效
		{HandlerName: "h3", MaxInFlight: 2}, // 与首值相等：不告警
	} {
		if err := b.attachHandler("order.first", h.HandlerName, noop, h); err != nil {
			t.Fatalf("attachHandler(%s): %v", h.HandlerName, err)
		}
	}

	sub := b.subs["order.first"]
	if sub == nil {
		t.Fatal("subscription not registered")
	}
	if sub.maxInFlight != 2 {
		t.Fatalf("effective max_in_flight = %d, want first handler value 2", sub.maxInFlight)
	}
	if len(sub.handlers) != 3 {
		t.Fatalf("handlers = %d, want 3", len(sub.handlers))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) != 1 || !strings.Contains(c.logs[0], "max_in_flight mismatch ignored") {
		t.Fatalf("warn logs = %v, want exactly 1 mismatch warn", c.logs)
	}
}

// captureLogs 收集日志记录（WK-14 测试用）。
type captureLogs struct {
	mu    sync.Mutex
	logs  []string
	level slog.Level
}

func (c *captureLogs) Enabled(_ context.Context, l slog.Level) bool { return l >= c.level }
func (c *captureLogs) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, r.Message)
	return nil
}
func (c *captureLogs) WithAttrs(attrs []slog.Attr) slog.Handler { return c }
func (c *captureLogs) WithGroup(name string) slog.Handler       { return c }

// TestForwardDeliveryAckGivesUpOnCtxCancel 回归 WK-14（复审-5 调整）：
// 消息永不 Ack/Nack 且订阅关停（ctx 取消）时，确认转达 goroutine 必须
// 放弃等待并记 Warn 退出，而非常驻泄漏。
func TestForwardDeliveryAckGivesUpOnCtxCancel(t *testing.T) {
	c := &captureLogs{level: slog.LevelWarn}
	b := New(eventbus.Options{})
	b.logger = slog.New(c)

	ctx, cancel := context.WithCancel(context.Background())
	msg := message.NewMessage("m1", nil)
	b.forwardDeliveryAck(ctx, msg, eventbus.Delivery{Ack: func() {}, Nack: func() {}}, nil)
	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.logs)
		c.mu.Unlock()
		if n > 0 {
			return // 关停放弃并记录了 Warn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("forwardDeliveryAck did not give up on ctx cancel; goroutine would leak forever")
}

// TestForwardDeliveryAckWaitsForSlowHandler 回归复审-5：确认转达不得设
// 人为超时——合法慢 handler 的 Ack 晚到（此处 100ms，量级可任意延长）
// 只要订阅仍在运行就必须转达，否则 AutoCommit=false 下 offset 永不提交、
// 消息重复消费（修复前 30s 固定上限会静默截断）。
func TestForwardDeliveryAckWaitsForSlowHandler(t *testing.T) {
	acked := make(chan struct{}, 1)
	c := &captureLogs{level: slog.LevelWarn}
	b := New(eventbus.Options{})
	b.logger = slog.New(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := message.NewMessage("m1", nil)
	b.forwardDeliveryAck(ctx, msg, eventbus.Delivery{
		Ack:  func() { acked <- struct{}{} },
		Nack: func() {},
	}, nil)
	time.Sleep(100 * time.Millisecond) // 模拟慢 handler 尚未返回
	msg.Ack()                          // 确认晚到

	select {
	case <-acked:
	case <-time.After(3 * time.Second):
		t.Fatal("late ack from a slow handler must still be forwarded while subscription is alive")
	}
	c.mu.Lock()
	n := len(c.logs)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("no warn expected while subscription is alive, got %d", n)
	}
}

// fakeNonMemoryTransport 是持久化 Transport 假件（订阅/发布行为无关紧要，
// 测试只驱动 Bus 内部路径）。
type fakeNonMemoryTransport struct{}

func (fakeNonMemoryTransport) Publish(ctx context.Context, topic string, e *eventbus.RawEvent) error {
	return nil
}
func (fakeNonMemoryTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan eventbus.Delivery, error) {
	return nil, nil
}
func (fakeNonMemoryTransport) Topics() []string { return nil }
func (fakeNonMemoryTransport) Close() error     { return nil }

// TestAttachHandlerFailureRollsBack 回归订阅失败回滚：attach 失败（此处
// router 未 Init，AddConsumerHandler panic 被翻译为错误）时必须回滚
// handlerNames 与订阅注册表——残留会锁死该事件（后续订阅挂到没有 router
// handler 的幽灵订阅上，永远收不到消息）。
func TestAttachHandlerFailureRollsBack(t *testing.T) {
	b := New(eventbus.Options{DefaultTransport: fakeNonMemoryTransport{}})
	forceStarted(b)

	err := b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h1"))
	if err == nil || errors.Is(err, errHandlerNameTaken) {
		t.Fatalf("err = %v, want non-taken attach failure", err)
	}
	b.mu.Lock()
	_, nameTaken := b.handlerNames["h1"]
	subCount := len(b.subs)
	b.mu.Unlock()
	if nameTaken {
		t.Fatal("rollback incomplete: handlerNames entry not removed")
	}
	if subCount != 0 {
		t.Fatalf("rollback incomplete: %d subscription(s) left behind", subCount)
	}

	// 复位 started 走 pending 路径：同一事件可再次订阅成功。
	b.mu.Lock()
	b.started = false
	b.mu.Unlock()
	if err := b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h2")); err != nil {
		t.Fatalf("resubscribe must succeed, got: %v", err)
	}
}

// TestAttachHandlerPanicErrorIncludesStack 回归复审-8：非 duplicate
// panic 翻译为错误时必须附带 debug.Stack()——仅 %v 的 panic 值无堆栈，
// 未知 panic 源无从排查。构造同上：router 为 nil 触发确定性 panic。
func TestAttachHandlerPanicErrorIncludesStack(t *testing.T) {
	b := New(eventbus.Options{DefaultTransport: fakeNonMemoryTransport{}})
	forceStarted(b)

	err := b.Subscribe(context.Background(), "order.created", func(context.Context, *eventbus.RawEvent) error { return nil }, eventbus.WithHandlerName("h1"))
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want translated panic error", err)
	}
	// debug.Stack() 输出以 "goroutine N [running]:" 开头。
	if !strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("panic error must include stack trace, got: %v", err)
	}
}

// TestFromConfigTopicOptionsMapping 钉住 bus.topics.<topic> 的完整映射：
// max_in_flight / auto_ack / continue_on_error 都必须落到
// eventbus.Options.Topics（max_in_flight 曾漏映射，yaml 键静默失效）。
// 消费组 / 成员数不进 Bus 配置——它们是 kafka 段（consumer.*）的事。
func TestFromConfigTopicOptionsMapping(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
bus:
  topics:
    orders:
      max_in_flight: 2
      handler_timeout: 3s
      auto_ack: true
      continue_on_error: true
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	bus, err := NewFromConfig(lynx.NewViperConfig(v), map[string]eventbus.Transport{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	tc := bus.opts.Topics["orders"]
	if tc.MaxInFlight != 2 || tc.HandlerTimeout != 3*time.Second || !tc.AutoAck || !tc.ContinueOnError {
		t.Fatalf("topic config = %+v, want max_in_flight/handler_timeout/auto_ack/continue_on_error mapped", tc)
	}
}

// TestFromConfigMaxRedeliveriesMapping 验证 WK-02 配置入口：
// bus.max_redeliveries / bus.topics.<topic>.max_redeliveries 装配到扩展 Options。
func TestFromConfigMaxRedeliveriesMapping(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
bus:
  max_redeliveries: 7
  topics:
    poison:
      max_redeliveries: 2
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	bus, err := NewFromConfig(lynx.NewViperConfig(v), map[string]eventbus.Transport{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if bus.ext.MaxRedeliveries != 7 {
		t.Fatalf("bus-level MaxRedeliveries = %d, want 7", bus.ext.MaxRedeliveries)
	}
	if got := bus.ext.Topics["poison"].MaxRedeliveries; got != 2 {
		t.Fatalf("topic-level MaxRedeliveries = %d, want 2", got)
	}
}
