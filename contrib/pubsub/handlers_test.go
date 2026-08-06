package pubsub

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
)

// startRouter 创建内存默认 Transport 的 Broker + Router，缓冲订阅并启动。
func startRouter(t *testing.T, handlers []Handler) (Broker, chan error) {
	t.Helper()
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	r := NewRouter(b, handlers)
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("broker Init: %v", err)
	}
	if err := r.Init(newFakeApp()); err != nil {
		t.Fatalf("router Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		cancel()
		t.Fatal("broker did not become healthy")
	}
	time.Sleep(200 * time.Millisecond) // 等待 gochannel 完成订阅接线

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

// TestRouterTypedHandler 验证类型化 handler：payload 经 topic 的 Marshaler
// 自动反序列化，元数据（ID/Key/Headers）经 TypedMessage 信封透传。
func TestRouterTypedHandler(t *testing.T) {
	type Order struct {
		ID  string
		Qty int
	}
	var got atomic.Value // *TypedMessage[Order]
	b, _ := startRouter(t, []Handler{
		NewHandler("orders", "orderHandler", func(ctx context.Context, event *TypedMessage[Order]) error {
			got.Store(event)
			return nil
		}),
	})

	if err := b.Publish(context.Background(), "orders", Order{ID: "A1", Qty: 3},
		WithMessageKey("k1"), WithMetadataField("foo", "bar")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return got.Load() != nil }) {
		t.Fatal("typed handler was not called")
	}
	ev := got.Load().(*TypedMessage[Order])
	if ev.Payload.ID != "A1" || ev.Payload.Qty != 3 {
		t.Fatalf("payload = %+v, want {A1 3}", ev.Payload)
	}
	if ev.ID == "" {
		t.Fatal("message ID is empty")
	}
	if ev.Key != "k1" {
		t.Fatalf("key = %q, want k1", ev.Key)
	}
	if ev.Headers["foo"] != "bar" {
		t.Fatalf("headers = %v, want foo=bar", ev.Headers)
	}
}

// TestRouterRawHandler 验证原始字节 handler：直接收到 *Message，payload 不被
// 序列化器处理。
func TestRouterRawHandler(t *testing.T) {
	var got atomic.Value // *Message
	b, _ := startRouter(t, []Handler{
		NewRawHandler("raw", "rawHandler", func(ctx context.Context, event *Message) error {
			got.Store(event)
			return nil
		}),
	})

	raw := []byte("{not-json")
	if err := b.Publish(context.Background(), "raw", NewMessage(raw), WithMessageKey("k2")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return got.Load() != nil }) {
		t.Fatal("raw handler was not called")
	}
	msg := got.Load().(*Message)
	if string(msg.Payload) != string(raw) {
		t.Fatalf("raw payload = %q, want preserved %q", msg.Payload, raw)
	}
	if msg.Key != "k2" {
		t.Fatalf("key = %q, want k2", msg.Key)
	}
}

// TestRouterMixedHandlers 验证类型化与原始 handler 可共存于同一 Router。
func TestRouterMixedHandlers(t *testing.T) {
	type Greeting struct {
		Text string
	}
	var typed atomic.Value // *TypedMessage[Greeting]
	var raw atomic.Value   // *Message
	b, _ := startRouter(t, []Handler{
		NewHandler("greet", "typedHandler", func(ctx context.Context, event *TypedMessage[Greeting]) error {
			typed.Store(event)
			return nil
		}),
		NewRawHandler("raw", "rawHandler", func(ctx context.Context, event *Message) error {
			raw.Store(event)
			return nil
		}),
	})

	if err := b.Publish(context.Background(), "greet", Greeting{Text: "hi"}); err != nil {
		t.Fatalf("Publish greet: %v", err)
	}
	if err := b.Publish(context.Background(), "raw", NewMessage([]byte("raw-data"))); err != nil {
		t.Fatalf("Publish raw: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return typed.Load() != nil && raw.Load() != nil }) {
		t.Fatal("handlers were not called")
	}
	if ev := typed.Load().(*TypedMessage[Greeting]); ev.Payload.Text != "hi" {
		t.Fatalf("typed payload = %+v, want {hi}", ev.Payload)
	}
	if msg := raw.Load().(*Message); string(msg.Payload) != "raw-data" {
		t.Fatalf("raw payload = %q, want raw-data", msg.Payload)
	}
}

// TestRouterTypedUnmarshalError 验证反序列化失败进入失败处理管线（不调用
// handler），与 Subscribe[T] 语义一致。
func TestRouterTypedUnmarshalError(t *testing.T) {
	var calls atomic.Int32
	b, _ := startRouter(t, []Handler{
		NewHandler("orders", "orderHandler", func(ctx context.Context, event *TypedMessage[string]) error {
			calls.Add(1)
			return nil
		}),
	})

	if err := b.Publish(context.Background(), "orders", NewMessage([]byte("{oops"))); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("handler called %d times despite unmarshal failure, want 0", n)
	}
}

// TestRouterHandlerOptionsApplied 验证工厂传入的订阅选项生效（WithAutoAck
// 下失败 handler 不被重试）。
func TestRouterHandlerOptionsApplied(t *testing.T) {
	var calls atomic.Int32
	b, _ := startRouter(t, []Handler{
		NewRawHandler("raw", "rawHandler", func(ctx context.Context, event *Message) error {
			calls.Add(1)
			return errors.New("boom")
		}, WithAutoAck()),
	})

	if err := b.Publish(context.Background(), "raw", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("handler was not called")
	}
	time.Sleep(500 * time.Millisecond) // 给 Retry 中间件留出触发窗口
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 (AutoAck must not trigger Retry)", got)
	}
}

// TestTypedMessageBytesIdentity 验证 []byte 的 Payload 身份解码：不经过
// Marshaler，原始字节透传（raw 语义），元数据一并填充。
func TestTypedMessageBytesIdentity(t *testing.T) {
	var ev TypedMessage[[]byte]
	msg := NewMessage([]byte("{not-json"), WithID("m1"), WithKey("k1"))
	if err := ev.Decode(JSONMarshaler{}, msg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(ev.Payload) != "{not-json" {
		t.Fatalf("payload = %q, want raw bytes preserved", ev.Payload)
	}
	if ev.ID != "m1" || ev.Key != "k1" {
		t.Fatalf("metadata lost: id=%q key=%q", ev.ID, ev.Key)
	}
}

// TestTypedSubscribeRawBytes 验证 Subscribe[[]byte] 的原始字节语义：
// 非法 JSON 也能原样送达 handler。
func TestTypedSubscribeRawBytes(t *testing.T) {
	ft := newFakeTransport("raw")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var got atomic.Value // *TypedMessage[[]byte]
	if err := Subscribe(b, context.Background(), "raw", "h1", func(ctx context.Context, event *TypedMessage[[]byte]) error {
		got.Store(event)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe typed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not become healthy")
	}

	ft.inject(message.NewMessage("m1", []byte("{oops")))
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return got.Load() != nil }) {
		t.Fatal("raw bytes handler was not called")
	}
	ev := got.Load().(*TypedMessage[[]byte])
	if string(ev.Payload) != "{oops" {
		t.Fatalf("payload = %q, want raw bytes preserved", ev.Payload)
	}
	if ev.ID != "m1" {
		t.Fatalf("id = %q, want m1", ev.ID)
	}
}
