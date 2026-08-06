package pubsub

import (
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
)

// fakeApp is a minimal lynx.App implementation for tests.
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

func (f *fakeApp) Close()                                    {}
func (f *fakeApp) Config() lynx.Config                       { return nil }
func (f *fakeApp) Context() context.Context                  { return f.ctx }
func (f *fakeApp) CLI(lynx.CommandFunc) error                { return nil }
func (f *fakeApp) OnStart(...lynx.HookFunc)                  {}
func (f *fakeApp) OnStop(...lynx.HookFunc)                   {}
func (f *fakeApp) Register(...lynx.Component)                {}
func (f *fakeApp) RegisterBuilders(...lynx.ComponentBuilder) {}
func (f *fakeApp) HealthCheckFunc() lynx.HealthCheckFunc     { return nil }
func (f *fakeApp) Run() error                                { return nil }
func (f *fakeApp) SetLogger(logger *slog.Logger)             { f.logger = logger }
func (f *fakeApp) Logger(kwargs ...any) *slog.Logger         { return f.logger.With(kwargs...) }

var _ lynx.App = (*fakeApp)(nil)

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
	subCh         chan *message.Message
}

func newFakeTransport(topics ...string) *fakeTransport {
	return &fakeTransport{topics: topics, subCh: make(chan *message.Message)}
}

func (f *fakeTransport) Name() string                { return "fake-transport" }
func (f *fakeTransport) Init(lynx.App) error         { return nil }
func (f *fakeTransport) Start(context.Context) error { return nil }
func (f *fakeTransport) Stop(context.Context)        {}
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
		b.Stop(context.Background())
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
	b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestBrokerStopBeforeStart 回归 P1-5：Stop 必须先于 Start 调用被容忍
//（Init 成功但 Start 未执行的失败清理路径）——不 panic，且随后正常
// Start/Stop 流程不受影响。
func TestBrokerStopBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	b.Stop(context.Background())
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
	b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
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
	b.Stop(context.Background())
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
	b.Stop(context.Background())
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
