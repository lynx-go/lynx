package pubsub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/logging"
)

// fakeApp is a minimal lynx.AppContext implementation for tests.
type fakeApp struct {
	ctx    context.Context
	logger *slog.Logger
}

func newFakeApp() *fakeApp {
	return &fakeApp{
		ctx:    context.Background(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newFakeAppWithBuffer 返回写入 buffer 的 fakeApp（验证日志行为用）。
func newFakeAppWithBuffer(buf *bytes.Buffer) *fakeApp {
	return &fakeApp{
		ctx: context.Background(),
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
}

func (f *fakeApp) Context() context.Context          { return f.ctx }
func (f *fakeApp) Config() lynx.Config               { return nil }
func (f *fakeApp) Logger(kwargs ...any) *slog.Logger { return f.logger.With(kwargs...) }
func (f *fakeApp) HealthCheckers() []lynx.Checker    { return nil }
func (f *fakeApp) Close()                            {}

var _ lynx.AppContext = (*fakeApp)(nil)

func pollUntil(deadline time.Duration, interval time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(interval)
	}
	return cond()
}

// fakeTransport 记录 Publish 调用并可注入订阅消息。
type fakeTransport struct {
	mu            sync.Mutex
	topics        []string
	published     []string
	publishedMsgs []*message.Message
	subOpts       []SubscriptionOptions
	subCh         chan *message.Message
}

func newFakeTransport(topics ...string) *fakeTransport {
	return &fakeTransport{topics: topics, subCh: make(chan *message.Message)}
}

func (f *fakeTransport) Name() string                { return "fake-transport" }
func (f *fakeTransport) Init(lynx.AppContext) error  { return nil }
func (f *fakeTransport) Start(context.Context) error { return nil }
func (f *fakeTransport) Stop(context.Context) error  { return nil }
func (f *fakeTransport) CheckHealth() error          { return nil }
func (f *fakeTransport) Topics() []string            { return f.topics }

func (f *fakeTransport) Publish(ctx context.Context, topic string, msgs ...*message.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, topic)
	f.publishedMsgs = append(f.publishedMsgs, msgs...)
	return nil
}

func (f *fakeTransport) Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error) {
	f.mu.Lock()
	f.subOpts = append(f.subOpts, opts)
	f.mu.Unlock()
	out := make(chan *message.Message)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-f.subCh:
				out <- msg
			}
		}
	}()
	return out, nil
}

func (f *fakeTransport) publishedTopics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.published...)
}

// publishedMessages 返回发布的消息副本（marshaller 测试用）。
func (f *fakeTransport) publishedMessages() []*message.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*message.Message(nil), f.publishedMsgs...)
}

// inject 向订阅者注入一条消息。
func (f *fakeTransport) inject(msg *message.Message) {
	f.subCh <- msg
}

// startBroker 创建内存默认 Transport 的 Broker，注册订阅并启动。
func startBroker(t *testing.T, h HandlerFunc, subOpts ...SubscribeOption) (Broker, chan error) {
	t.Helper()
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(context.Background(), "test.event", "test-handler", h, subOpts...); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		cancel()
		t.Fatalf("broker did not become healthy")
	}
	// 等待 gochannel 完成订阅接线。
	time.Sleep(200 * time.Millisecond)

	t.Cleanup(func() {
		cancel()
		_ = b.Stop(context.Background())
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("broker did not stop within 3s")
		}
	})
	return b, done
}

func TestBrokerBeforeInit(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error before Init")
	}
}

func TestBrokerPublishSubscribe(t *testing.T) {
	type result struct {
		ctx context.Context
		msg *Message
	}
	received := make(chan result, 1)

	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		received <- result{ctx: ctx, msg: msg}
		return nil
	})

	published := MustJSONMessage(map[string]string{"hello": "world"})
	if err := b.Publish(context.Background(), "test.event", published,
		WithMessageKey("key-1"),
		WithMetadataField("foo", "bar"),
	); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case r := <-received:
		if string(r.msg.Payload) != string(published.Payload) {
			t.Errorf("payload = %s, want %s", r.msg.Payload, published.Payload)
		}
		if r.msg.Key != "key-1" {
			t.Errorf("key = %q, want key-1", r.msg.Key)
		}
		if r.msg.Headers["foo"] != "bar" {
			t.Errorf("header foo = %q, want bar", r.msg.Headers["foo"])
		}
		if got := MessageIDFromContext(r.ctx); got != published.ID {
			t.Errorf("MessageIDFromContext() = %q, want %q", got, published.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive published message within 5s")
	}
}

func TestBrokerStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(ctx, "noop.event", "noop-handler", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not start")
	}
	cancel()
	_ = b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestBrokerStopBeforeStart 回归 P1-5：Stop 必须先于 Start 调用被容忍
// （Init 成功但 Start 未执行的失败清理路径）——不 panic，且随后正常
// Start/Stop 流程不受影响。
func TestBrokerStopBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = b.Stop(context.Background())
	// Stop 后再走正常生命周期：不得因提前 Stop 破坏 Start。
	if err := b.Subscribe(ctx, "noop.event", "noop-handler", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not start after Stop-before-Start")
	}
	cancel()
	_ = b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestBrokerStopAfterStartBlocksSubscribe 回归 P3-3：真实 Start→Stop 后
// 生命周期终结，Subscribe 必须返回明确的 stopped 错误（此前 started 仍为
// true，报 "cannot subscribe to a started broker" 误导文案）；Start 同样
// 报 stopped 而非 started。
func TestBrokerStopAfterStartBlocksSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not start")
	}
	cancel()
	_ = b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	err := b.Subscribe(ctx, "late.event", "late-handler", func(ctx context.Context, msg *Message) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Subscribe after Stop error = %v, want stopped-broker error", err)
	}
	if err := b.Start(ctx); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Start after Stop error = %v, want stopped-broker error", err)
	}
}

// TestBrokerPublishTypedNilMessage 回归 P2-5：类型断言的 typed nil *Message
// payload 必须返回明确错误，不得在 cloneMessage 中解引用 panic。
func TestBrokerPublishTypedNilMessage(t *testing.T) {
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var nilMsg *Message
	err := b.Publish(context.Background(), "test.event", nilMsg)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("Publish(typed nil *Message) error = %v, want explicit nil error", err)
	}
}

// TestBrokerRouteKeyTranslatesTopic 验证 RouteKey：逻辑 topic 经 key 别名
// 转发到 transport 侧主题名——Publish/Subscribe 两侧都必须用 key 调用
// transport，否则发布与订阅落在不同的后端主题上（对 gochannel 即
// 精确匹配不同 channel，消息必然丢失）。
func TestBrokerRouteKeyTranslatesTopic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	memT := NewMemoryTransport()
	b := NewBroker(Options{DefaultTransport: memT})
	// 逻辑 topic "orders" → 内存 transport，后端主题名 "orders_v1"。
	b.RouteKey("orders", memT, "orders_v1")
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	received := make(chan *Message, 1)
	if err := b.Subscribe(ctx, "orders", "h1", func(ctx context.Context, msg *Message) error {
		received <- msg
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not start")
	}

	// 直接向内存 transport 订阅后端主题名：若 broker 未翻译 key，这里收不到。
	backendCh, err := memT.Subscribe(ctx, "orders_v1", SubscriptionOptions{})
	if err != nil {
		t.Fatalf("backend Subscribe: %v", err)
	}

	if err := b.Publish(ctx, "orders", MustJSONMessage(map[string]string{"id": "1"})); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive message via translated key")
	}
	select {
	case <-backendCh:
	case <-time.After(3 * time.Second):
		t.Fatal("backend topic did not receive message: broker did not translate key")
	}

	cancel()
	_ = b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestBrokerRouteKeyOverridesAutoRoute 验证 RouteKey 同样覆盖自动路由
// （显式路由语义与 Route 一致）。
func TestBrokerRouteKeyOverridesAutoRoute(t *testing.T) {
	memT := NewMemoryTransport()
	conflictT := newFakeTransport("auto")
	b := NewBroker(Options{Transports: []Transport{conflictT}})
	b.RouteKey("auto", memT, "auto_v1")
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 显式路由覆盖后，自动路由不得因"多 transport 路由同一 topic"报错。
	gotT, gotKey, err := b.(*broker).resolve("auto")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotT != memT || gotKey != "auto_v1" {
		t.Fatalf("resolve = (%v, %q), want (memT, auto_v1)", gotT, gotKey)
	}
}

func TestBrokerRetriesFailedHandler(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		if calls.Add(1) >= 2 {
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return errors.New("handler failed")
	})

	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler was not retried (calls=%d)", calls.Load())
	}
}

func TestBrokerContinueOnErrorAcks(t *testing.T) {
	var calls atomic.Int32
	receivedSecond := make(chan struct{}, 1)
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		if calls.Add(1) == 1 {
			return errors.New("first message fails")
		}
		select {
		case receivedSecond <- struct{}{}:
		default:
		}
		return nil
	}, WithContinueOnError())

	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish first: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("first message was not processed")
	}
	if err := b.CheckHealth(); err != nil {
		t.Fatalf("broker unhealthy after failing handler: %v", err)
	}
	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish second: %v", err)
	}
	select {
	case <-receivedSecond:
	case <-time.After(5 * time.Second):
		t.Fatal("second message was not delivered after ContinueOnError")
	}
}

func TestBrokerRouteExplicitAndFallback(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	b.Route("orders", ft)

	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish routed: %v", err)
	}
	if got := ft.publishedTopics(); len(got) != 1 || got[0] != "orders" {
		t.Fatalf("expected routed publish, got %v", got)
	}

	// 未命中路由表的 topic 走默认 Transport（内存）——不报错。
	if err := b.Publish(context.Background(), "local.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish fallback: %v", err)
	}
}

func TestBrokerPublishNoTransport(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err == nil {
		t.Fatal("expected Publish error with no route and no default transport")
	}
}

func TestBrokerSubscribeNoTransportFailsAtStart(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(context.Background(), "orders", "h", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe buffered: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected Start error for un-routed subscription")
	}
}

func TestBrokerAutoRouteFromTransports(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish auto-routed: %v", err)
	}
	if got := ft.publishedTopics(); len(got) != 1 {
		t.Fatalf("expected auto-routed publish, got %v", got)
	}
}

func TestBrokerAutoRouteConflict(t *testing.T) {
	ft1 := newFakeTransport("orders")
	ft2 := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft1, ft2}})
	if err := b.Init(newFakeApp()); err == nil {
		t.Fatal("expected Init error for conflicting auto routes")
	}
}

func TestBrokerSubscribeAfterStartFails(t *testing.T) {
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error { return nil })
	if err := b.Subscribe(context.Background(), "late.event", "late-handler", func(ctx context.Context, msg *Message) error {
		return nil
	}); err == nil {
		t.Fatal("expected Subscribe error after Start")
	}
}

func TestBrokerExplicitRouteOverridesAutoRoute(t *testing.T) {
	ft1 := newFakeTransport("orders")
	ft2 := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft2}})
	b.Route("orders", ft1) // 显式 Route 先于 Init 自动路由
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init must not conflict when explicit Route precedes auto route: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := ft1.publishedTopics(); len(got) != 1 {
		t.Fatalf("expected publish routed to explicit transport ft1, got %v", got)
	}
	if got := ft2.publishedTopics(); len(got) != 0 {
		t.Fatalf("expected no publish to auto-routed transport ft2, got %v", got)
	}
}

func TestBrokerSubscribeDuplicateHandlerName(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Subscribe(context.Background(), "t1", "dup", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	if err := b.Subscribe(context.Background(), "t2", "dup", func(ctx context.Context, msg *Message) error {
		return nil
	}); err == nil {
		t.Fatal("expected duplicate handler name error")
	}
	if err := b.Subscribe(context.Background(), "t3", "", func(ctx context.Context, msg *Message) error {
		return nil
	}); err == nil {
		t.Fatal("expected empty handler name error")
	}
}

func TestBrokerStartSecondSubscriptionFailureRetry(t *testing.T) {
	// C2 回归：第二条订阅 resolve 失败（部分注册）后补充 Route 重试，
	// 不再触发 watermill DuplicateHandlerNameError panic。
	ft := newFakeTransport("known")
	b := NewBroker(Options{})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	b.Route("known", ft) // 第一条可注册，第二条失败才是"部分注册"场景
	for _, s := range []struct{ topic, name string }{
		{"known", "h1"},
		{"unknown", "h2"},
	} {
		if err := b.Subscribe(context.Background(), s.topic, s.name, func(ctx context.Context, msg *Message) error {
			return nil
		}); err != nil {
			t.Fatalf("Subscribe %s: %v", s.name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected Start error for un-routed second subscription")
	}

	// 补充 Route 后重试（旧实现因 h1 已注册残留而 panic）。
	b.Route("unknown", ft)
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not become healthy after re-Start")
	}
	if err := b.Publish(context.Background(), "known", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := ft.publishedTopics(); len(got) != 1 {
		t.Fatalf("expected routed publish after recovery, got %v", got)
	}
	cancel()
	_ = b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

func TestBrokerStartBeforeInitFails(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("expected error when Start is called before Init")
	}
}

func TestBrokerStartFailureRecovery(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(context.Background(), "orders", "h", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected Start error for un-routed subscription")
	}

	// Start 失败后状态可恢复：补充 Route 后重新 Start 成功。
	ft := newFakeTransport("orders")
	b.Route("orders", ft)
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not become healthy after re-Start")
	}
	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := ft.publishedTopics(); len(got) != 1 {
		t.Fatalf("expected routed publish after recovery, got %v", got)
	}
	cancel()
	_ = b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

func TestBrokerAutoAckNoRetry(t *testing.T) {
	var calls atomic.Int32
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		calls.Add(1)
		return errors.New("handler failed")
	}, WithAutoAck())

	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("first message was not processed")
	}
	// 给 Retry 中间件留出触发窗口：AutoAck 下失败 handler 不得被重试。
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 (AutoAck must not trigger Retry)", got)
	}
	// 消息已确认，不阻塞后续消息。
	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish second: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() == 2 }) {
		t.Fatalf("second message was not processed after AutoAck failure (calls=%d)", calls.Load())
	}
}

func TestBrokerPublishDoesNotMutateMessage(t *testing.T) {
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	msg := MustJSONMessage(map[string]string{"a": "b"})
	msg.Key = "original-key"
	msg.Headers = map[string]string{"k": "v"}
	if err := b.Publish(context.Background(), "orders", msg,
		WithMessageKey("override"),
		WithMetadata(map[string]string{"k2": "v2"}),
	); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if msg.Key != "original-key" {
		t.Errorf("msg.Key mutated by Publish: %q", msg.Key)
	}
	if _, ok := msg.Headers["k2"]; ok {
		t.Error("msg.Headers mutated by Publish")
	}
	if msg.Headers["k"] != "v" {
		t.Errorf("original header lost: %v", msg.Headers)
	}
}

func (f *fakeTransport) publishedSubOptions() []SubscriptionOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SubscriptionOptions(nil), f.subOpts...)
}

// startBrokerWithEvents 创建指定 Options 的 Broker（测试事件级选项），
// 注册订阅并启动。
func startBrokerWithEvents(t *testing.T, opts Options, topic, handlerName string, h HandlerFunc, subOpts ...SubscribeOption) (Broker, chan error) {
	t.Helper()
	b := NewBroker(opts)
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(context.Background(), topic, handlerName, h, subOpts...); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		cancel()
		t.Fatalf("broker did not become healthy")
	}
	time.Sleep(200 * time.Millisecond) // 等待订阅接线完成

	t.Cleanup(func() {
		cancel()
		_ = b.Stop(context.Background())
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("broker did not stop within 3s")
		}
	})
	return b, done
}

// TestBrokerEventOptionsSubscribeDefaults 验证事件级选项作为 Subscribe 默认值：
// group/instances 透传到 transport，auto_ack 使失败 handler 不重试。
func TestBrokerEventOptionsSubscribeDefaults(t *testing.T) {
	ft := newFakeTransport("test.event")
	startBrokerWithEvents(t, Options{
		Transports:       []Transport{ft},
		DefaultTransport: ft,
		Events: map[string]EventOptions{"test.event": {
			AutoAck: true, Group: "orders-group", Instances: 2,
		}},
	}, "test.event", "test-handler", func(ctx context.Context, msg *Message) error {
		return errors.New("handler failed")
	})
	if got := ft.publishedSubOptions(); len(got) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(got))
	} else if got[0].Group != "orders-group" || got[0].Instances != 2 {
		t.Fatalf("subscription options = %+v, want group=orders-group instances=2", got[0])
	}

	var calls atomic.Int32
	b2, _ := startBrokerWithEvents(t, Options{
		DefaultTransport: NewMemoryTransport(),
		Events:           map[string]EventOptions{"test.event": {AutoAck: true}},
	}, "test.event", "test-handler2", func(ctx context.Context, msg *Message) error {
		calls.Add(1)
		return errors.New("handler failed")
	})
	if err := b2.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("first message was not processed")
	}
	time.Sleep(500 * time.Millisecond) // 给重试留出触发窗口
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 (event AutoAck must not trigger retry)", got)
	}
}

// TestBrokerEventRetryNoRetry 验证事件级 retry {max_retries: 0} 不重试：
// 失败 handler 仅执行一次（fake transport 不重投，消息 Nack 后丢弃）。
func TestBrokerEventRetryNoRetry(t *testing.T) {
	var calls atomic.Int32
	zero := 0
	ft := newFakeTransport("test.event")
	startBrokerWithEvents(t, Options{
		Transports:       []Transport{ft},
		DefaultTransport: ft,
		Events:           map[string]EventOptions{"test.event": {Retry: &RetryOptions{MaxRetries: zero}}},
	}, "test.event", "test-handler", func(ctx context.Context, msg *Message) error {
		calls.Add(1)
		return errors.New("handler failed")
	})
	ft.inject(message.NewMessage("m1", []byte("x")))
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("message was not processed")
	}
	time.Sleep(500 * time.Millisecond) // 给重试留出触发窗口
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 (event retry 0 must not retry)", got)
	}
}

// TestBrokerDefaultRetryCount 验证缺省重试（无事件配置，{MaxRetries: 3}）：
// 失败 handler 共执行 4 次（1 次初始 + 3 次重试）。
func TestBrokerDefaultRetryCount(t *testing.T) {
	var calls atomic.Int32
	ft := newFakeTransport("test.event")
	startBrokerWithEvents(t, Options{
		Transports:       []Transport{ft},
		DefaultTransport: ft,
	}, "test.event", "test-handler", func(ctx context.Context, msg *Message) error {
		calls.Add(1)
		return errors.New("handler failed")
	})
	ft.inject(message.NewMessage("m1", []byte("x")))
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 4 }) {
		t.Fatalf("handler called %d times, want 4 (1 initial + 3 default retries)", calls.Load())
	}
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != 4 {
		t.Fatalf("handler called %d times, want exactly 4", got)
	}
}

// TestBrokerEventRetryOverride 验证事件级 retry 覆盖全局 Options.Retry
// （max_retries: 1 → 失败 handler 共执行 2 次）。
func TestBrokerEventRetryOverride(t *testing.T) {
	var calls atomic.Int32
	one := 1
	ft := newFakeTransport("test.event")
	startBrokerWithEvents(t, Options{
		Transports:       []Transport{ft},
		DefaultTransport: ft,
		Retry:            &RetryOptions{MaxRetries: 5},
		Events:           map[string]EventOptions{"test.event": {Retry: &RetryOptions{MaxRetries: one}}},
	}, "test.event", "test-handler", func(ctx context.Context, msg *Message) error {
		calls.Add(1)
		return errors.New("handler failed")
	})
	ft.inject(message.NewMessage("m1", []byte("x")))
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 2 }) {
		t.Fatalf("handler called %d times, want 2 (1 initial + 1 event retry)", calls.Load())
	}
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler called %d times, want exactly 2 (event retry must override global)", got)
	}
}

// TestBrokerWatermillDebugLog 验证 Options.Debug 控制 watermill 核心（router）
// debug 日志：关闭（缺省）时过滤为 info+，订阅接线等内部 debug 日志不输出；
// 开启时按应用日志级别输出。
func TestBrokerWatermillDebugLog(t *testing.T) {
	run := func(debug bool) string {
		var buf bytes.Buffer
		b := NewBroker(Options{DefaultTransport: NewMemoryTransport(), Debug: debug})
		if err := b.Init(newFakeAppWithBuffer(&buf)); err != nil {
			t.Fatalf("Init (debug=%v): %v", debug, err)
		}
		if err := b.Subscribe(context.Background(), "test.event", "test-handler", func(ctx context.Context, msg *Message) error {
			return nil
		}); err != nil {
			t.Fatalf("Subscribe (debug=%v): %v", debug, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- b.Start(ctx) }()
		if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
			cancel()
			t.Fatalf("broker did not become healthy (debug=%v)", debug)
		}
		time.Sleep(300 * time.Millisecond) // 等待订阅接线（router debug 日志在此输出）
		cancel()
		_ = b.Stop(context.Background())
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("broker did not stop (debug=%v)", debug)
		}
		return buf.String()
	}

	off := run(false)
	if strings.Contains(off, "Subscribing to topic") {
		t.Errorf("watermill debug log leaked with Debug=false:\n%s", off)
	}
	if !strings.Contains(off, "Running router handlers") {
		t.Errorf("watermill info log should pass through with Debug=false:\n%s", off)
	}

	on := run(true)
	if !strings.Contains(on, "Subscribing to topic") {
		t.Errorf("watermill debug log should show with Debug=true:\n%s", on)
	}
}

// TestBrokerPropagatesLogAttrs 验证 Publish 自动将 ctx 日志属性白名单
// （默认 {request_id, user_id}）写入消息头；白名单外的属性不传播。
func TestBrokerPropagatesLogAttrs(t *testing.T) {
	ft := newFakeTransport("test.event")
	b := NewBroker(Options{Transports: []Transport{ft}, DefaultTransport: ft})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "rid-1"),
		slog.String(logging.FieldUserID, "u1"),
		slog.String("secret", "x"),
	)
	if err := b.Publish(ctx, "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgs := ft.publishedMessages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if got := m.Metadata.Get(logging.FieldRequestID); got != "rid-1" {
		t.Errorf("request_id header = %q, want rid-1", got)
	}
	if got := m.Metadata.Get(logging.FieldUserID); got != "u1" {
		t.Errorf("user_id header = %q, want u1", got)
	}
	if _, ok := m.Metadata["secret"]; ok {
		t.Error("non-whitelisted attr leaked into message metadata")
	}
}

// TestBrokerPropagateCustomAndDisabled 验证 PropagateAttrs 自定义白名单与
// 非 nil 空切片关闭传播。
func TestBrokerPropagateCustomAndDisabled(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
		want map[string]string
	}{
		{name: "custom", keys: []string{logging.FieldRequestID}, want: map[string]string{logging.FieldRequestID: "rid-1"}},
		{name: "disabled", keys: []string{}, want: map[string]string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ft := newFakeTransport("test.event")
			b := NewBroker(Options{
				Transports:       []Transport{ft},
				DefaultTransport: ft,
				PropagateAttrs:   tt.keys,
			})
			if err := b.Init(newFakeApp()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			ctx := logging.WithAttrs(context.Background(),
				slog.String(logging.FieldRequestID, "rid-1"),
				slog.String(logging.FieldUserID, "u1"),
			)
			if err := b.Publish(ctx, "test.event", MustJSONMessage(nil)); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			msgs := ft.publishedMessages()
			if len(msgs) != 1 {
				t.Fatalf("published %d messages, want 1", len(msgs))
			}
			got := map[string]string{}
			for k, v := range msgs[0].Metadata {
				got[k] = v
			}
			if len(got) != len(tt.want) {
				t.Errorf("metadata = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("metadata[%s] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestBrokerPropagateDoesNotOverrideMessageHeader 验证 *Message 载荷自身
// 已设置的 header 不被 ctx 日志属性覆盖。
func TestBrokerPropagateDoesNotOverrideMessageHeader(t *testing.T) {
	ft := newFakeTransport("test.event")
	b := NewBroker(Options{Transports: []Transport{ft}, DefaultTransport: ft})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	msg := MustJSONMessage(nil)
	msg.Headers = map[string]string{logging.FieldRequestID: "explicit"}
	ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "auto"))
	if err := b.Publish(ctx, "test.event", msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgs := ft.publishedMessages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	if got := msgs[0].Metadata.Get(logging.FieldRequestID); got != "explicit" {
		t.Errorf("request_id header = %q, want explicit (message header wins)", got)
	}
}

// TestBrokerRestoresLogAttrsOnSubscribe 验证 Subscribe 侧从消息头还原
// 白名单字段到 handler ctx（本地已存在的 key 不被覆盖）。
func TestBrokerRestoresLogAttrsOnSubscribe(t *testing.T) {
	ft := newFakeTransport("test.event")
	received := make(chan context.Context, 1)
	startBrokerWithEvents(t, Options{
		Transports:       []Transport{ft},
		DefaultTransport: ft,
	}, "test.event", "test-handler", func(ctx context.Context, msg *Message) error {
		received <- ctx
		return nil
	})

	wm := message.NewMessage("m1", []byte("x"))
	wm.Metadata.Set(logging.FieldRequestID, "rid-1")
	wm.Metadata.Set(logging.FieldUserID, "u1")
	ft.inject(wm)

	select {
	case ctx := <-received:
		attrs := map[string]string{}
		for _, a := range logging.AttrsFrom(ctx) {
			attrs[a.Key] = a.Value.String()
		}
		if attrs[logging.FieldRequestID] != "rid-1" {
			t.Errorf("request_id restored = %q, want rid-1", attrs[logging.FieldRequestID])
		}
		if attrs[logging.FieldUserID] != "u1" {
			t.Errorf("user_id restored = %q, want u1", attrs[logging.FieldUserID])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive message within 3s")
	}
}

// TestBrokerLogAttrsRoundTrip 端到端验证：Publish ctx 属性 → 消息头 →
// 消费 handler ctx 还原，且 InfoContext 日志携带 request_id。
func TestBrokerLogAttrsRoundTrip(t *testing.T) {
	received := make(chan context.Context, 1)
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		received <- ctx
		return nil
	})

	ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "rid-1"))
	if err := b.Publish(ctx, "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case rctx := <-received:
		got := ""
		for _, a := range logging.AttrsFrom(rctx) {
			if a.Key == logging.FieldRequestID {
				got = a.Value.String()
			}
		}
		if got != "rid-1" {
			t.Errorf("request_id restored = %q, want rid-1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive message within 5s")
	}
}

// TestBrokerLogAttrsFlowIntoLogs 验证消费侧 handler 的 InfoContext 日志
// 在 NewAttrsHandler 包装下自动携带还原的 request_id（全链路日志闭环）。
func TestBrokerLogAttrsFlowIntoLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(logging.NewAttrsHandler(slog.NewJSONHandler(&buf, nil)))

	received := make(chan struct{}, 1)
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		logger.InfoContext(ctx, "handling message")
		received <- struct{}{}
		return nil
	})

	ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "rid-1"))
	if err := b.Publish(ctx, "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not receive message within 5s")
	}
	if !strings.Contains(buf.String(), `"request_id":"rid-1"`) {
		t.Errorf("handler log missing request_id, got: %s", buf.String())
	}
}
