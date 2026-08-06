package pubsub

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
)

// TestPublishDataAutoMarshals 验证 Publish 传业务对象时经默认 JSON
// marshaller 自动序列化。
func TestPublishDataAutoMarshals(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	data := map[string]string{"user": "alice"}
	if err := b.Publish(context.Background(), "orders", data); err != nil {
		t.Fatalf("Publish data: %v", err)
	}
	msgs := ft.publishedMessages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	var got map[string]string
	if err := json.Unmarshal(msgs[0].Payload, &got); err != nil {
		t.Fatalf("payload is not JSON: %v (%q)", err, msgs[0].Payload)
	}
	if got["user"] != "alice" {
		t.Fatalf("payload = %v, want user=alice", got)
	}
}

// TestPublishMessagePreserved 验证 payload 为 *Message 时走原语义：
// Payload 不被再次序列化，元数据选项仍生效。
func TestPublishMessagePreserved(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	raw := []byte("{not-json")
	if err := b.Publish(context.Background(), "orders", NewMessage(raw), WithMessageKey("k1")); err != nil {
		t.Fatalf("Publish message: %v", err)
	}
	msgs := ft.publishedMessages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	if string(msgs[0].Payload) != string(raw) {
		t.Fatalf("Payload was re-marshaled: %q, want %q", msgs[0].Payload, raw)
	}
	if msgs[0].Metadata.Get(MessageKeyKey.String()) != "k1" {
		t.Fatal("message key option was not applied")
	}
}

// TestTypedSubscribe 验证类型化订阅自动反序列化并透传元数据。
func TestTypedSubscribe(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	type Order struct {
		ID  string
		Qty int
	}
	var got atomic.Value // *TypedMessage[Order]
	if err := Subscribe(b, context.Background(), "orders", "h1", func(ctx context.Context, event *TypedMessage[Order]) error {
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

	wm := message.NewMessage("m1", []byte(`{"ID":"A1","Qty":3}`))
	wm.Metadata.Set(MessageKeyKey.String(), "k1")
	ft.inject(wm)

	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return got.Load() != nil }) {
		t.Fatal("typed handler was not called")
	}
	ev := got.Load().(*TypedMessage[Order])
	if ev.Payload.ID != "A1" || ev.Payload.Qty != 3 {
		t.Fatalf("payload = %+v, want {A1 3}", ev.Payload)
	}
	if ev.ID != "m1" || ev.Key != "k1" {
		t.Fatalf("metadata lost: id=%q key=%q", ev.ID, ev.Key)
	}

	cancel()
	b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestTypedSubscribeUnmarshalError 验证反序列化失败返回错误（进入重试/重投
// 管线），而非静默跳过。
func TestTypedSubscribeUnmarshalError(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var calls atomic.Int32
	if err := Subscribe(b, context.Background(), "orders", "h1", func(ctx context.Context, event *TypedMessage[string]) error {
		calls.Add(1)
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

	// 注入非法 JSON：Unmarshal 失败 → handler 不应被调用。
	ft.inject(message.NewMessage("bad", []byte("{oops")))
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("handler called %d times despite unmarshal failure, want 0", n)
	}

	cancel()
	b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// prefixMarshaler 是自定义序列化器：字符串前后缀协议。
type prefixMarshaler struct{}

func (prefixMarshaler) Marshal(v any) ([]byte, error) {
	return []byte("p:" + v.(string)), nil
}

func (prefixMarshaler) Unmarshal(data []byte, out any) error {
	s := string(data)
	if !strings.HasPrefix(s, "p:") {
		return &json.SyntaxError{}
	}
	*out.(*string) = s[2:]
	return nil
}

// TestTopicMarshalersOverride 验证按 topic 注入不同序列化器：
// 命中的 topic 用专属 marshaller，未命中回退默认。
func TestTopicMarshalersOverride(t *testing.T) {
	ft := newFakeTransport("orders", "audit")
	b := NewBroker(Options{
		Transports: []Transport{ft},
		TopicMarshalers: map[string]Marshaler{
			"audit": prefixMarshaler{},
		},
	})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// audit 命中专属 marshaller。
	if err := b.Publish(context.Background(), "audit", "hello"); err != nil {
		t.Fatalf("Publish audit: %v", err)
	}
	// orders 未命中，回退 JSON 默认。
	if err := b.Publish(context.Background(), "orders", map[string]string{"user": "alice"}); err != nil {
		t.Fatalf("Publish orders: %v", err)
	}
	msgs := ft.publishedMessages()
	if len(msgs) != 2 {
		t.Fatalf("published %d messages, want 2", len(msgs))
	}
	if got := string(msgs[0].Payload); got != "p:hello" {
		t.Fatalf("audit payload = %q, want %q (topic marshaler not used)", got, "p:hello")
	}
	var got map[string]string
	if err := json.Unmarshal(msgs[1].Payload, &got); err != nil {
		t.Fatalf("orders payload is not JSON: %v (%q)", err, msgs[1].Payload)
	}
	if got["user"] != "alice" {
		t.Fatalf("orders payload = %v, want user=alice", got)
	}
}

// TestCustomMarshaler 验证 Options.Marshaler 覆盖默认 JSON。
func TestCustomMarshaler(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}, Marshaler: prefixMarshaler{}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", "hello"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgs := ft.publishedMessages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	if got := string(msgs[0].Payload); got != "p:hello" {
		t.Fatalf("payload = %q, want %q (custom marshaler not used)", got, "p:hello")
	}
}
