package kafka_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"
	"github.com/lynx-go/lynx/eventbus"
)

// blackboxFactory 是 ClientFactory 的 fake：记录构造参数并返回可控的
// publisher/subscriber——经公开构造器 WithClientFactory 注入。
type blackboxFactory struct {
	pub *recordingPublisher
	sub *controllableSubscriber

	mu        sync.Mutex
	subParams wmkafka.SubscriberParams
	pubCalls  int
	subCalls  int
}

func newBlackboxFactory() *blackboxFactory {
	return &blackboxFactory{
		pub: &recordingPublisher{},
		sub: &controllableSubscriber{chans: map[string][]chan *message.Message{}},
	}
}

func (f *blackboxFactory) NewPublisher(_ []string, _ *sarama.Config, _ watermill.LoggerAdapter) (message.Publisher, error) {
	f.mu.Lock()
	f.pubCalls++
	f.mu.Unlock()
	return f.pub, nil
}

func (f *blackboxFactory) NewSubscriber(p wmkafka.SubscriberParams, _ watermill.LoggerAdapter) (message.Subscriber, error) {
	f.mu.Lock()
	f.subCalls++
	f.subParams = p
	f.mu.Unlock()
	return f.sub, nil
}

func (f *blackboxFactory) params() wmkafka.SubscriberParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subParams
}

type recordingPublisher struct {
	mu     sync.Mutex
	topics []string
	msgs   []*message.Message
	closed int
}

func (p *recordingPublisher) Publish(topic string, msgs ...*message.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, topic)
	p.msgs = append(p.msgs, msgs...)
	return nil
}

func (p *recordingPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	return nil
}

func (p *recordingPublisher) lastTopic() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.topics) == 0 {
		return ""
	}
	return p.topics[len(p.topics)-1]
}

type controllableSubscriber struct {
	mu     sync.Mutex
	chans  map[string][]chan *message.Message
	closed int
}

func (s *controllableSubscriber) Subscribe(_ context.Context, topic string) (<-chan *message.Message, error) {
	ch := make(chan *message.Message, 4)
	s.mu.Lock()
	s.chans[topic] = append(s.chans[topic], ch)
	s.mu.Unlock()
	return ch, nil
}

func (s *controllableSubscriber) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *controllableSubscriber) channel(topic string) chan *message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chans[topic]) == 0 {
		return nil
	}
	return s.chans[topic][0]
}

func (s *controllableSubscriber) count(topic string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chans[topic])
}

// ordersOptions 是黑盒用例的共享配置：一个发布 topic + 一个订阅 topic。
func ordersOptions() wmkafka.Options {
	return wmkafka.Options{Topics: map[string]wmkafka.TopicOptions{
		"orders": {
			Brokers:  []string{"127.0.0.1:9092"},
			Topics:   []string{"orders.v1"},
			Producer: &wmkafka.ProducerOptions{Topic: "orders.v1"},
			Consumer: &wmkafka.ConsumerOptions{GroupID: "orders-group", Instances: 1},
		},
	}}
}

func TestLifecycle(t *testing.T) {
	f := newBlackboxFactory()
	tr, err := wmkafka.NewTransport(ordersOptions(), wmkafka.WithClientFactory(f))
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if err := tr.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	startCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tr.Start(startCtx) }()

	select {
	case <-tr.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready not closed after Start")
	}
	if err := tr.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth while running = %v, want nil", err)
	}

	if err := tr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := tr.CheckHealth(); err == nil {
		t.Fatal("CheckHealth after Stop = nil, want error")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after Stop = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	// Stop 后发布/订阅一律拒绝。
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{}); err == nil ||
		!strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Publish after Stop = %v, want stopped", err)
	}
	if _, err := tr.Subscribe(context.Background(), "orders", eventbus.SubscribeOptions{}); err == nil ||
		!strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Subscribe after Stop = %v, want stopped", err)
	}
}

func TestPublish(t *testing.T) {
	f := newBlackboxFactory()
	tr, err := wmkafka.NewTransport(ordersOptions(), wmkafka.WithClientFactory(f))
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{
		ID: "e1", Key: "k1", Payload: []byte(`{"id":1}`),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := f.pub.lastTopic(); got != "orders.v1" {
		t.Fatalf("physical topic = %q, want orders.v1", got)
	}
	f.pub.mu.Lock()
	msg := f.pub.msgs[len(f.pub.msgs)-1]
	f.pub.mu.Unlock()
	if string(msg.Payload) != `{"id":1}` {
		t.Fatalf("payload = %q", msg.Payload)
	}
	if got := msg.Metadata.Get("x-message-key"); got != "k1" {
		t.Fatalf("x-message-key = %q, want k1", got)
	}
}

func TestSubscribeFanIn(t *testing.T) {
	opts := ordersOptions()
	to := opts.Topics["orders"]
	to.Topics = []string{"orders.v1", "orders.v2"}
	to.Consumer.Instances = 2
	opts.Topics["orders"] = to

	f := newBlackboxFactory()
	tr, err := wmkafka.NewTransport(opts, wmkafka.WithClientFactory(f))
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	deliveries, err := tr.Subscribe(context.Background(), "orders", eventbus.SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// 2 物理 topic × 2 实例 = 4 条子订阅。
	for _, physical := range []string{"orders.v1", "orders.v2"} {
		if got := f.sub.count(physical); got != 2 {
			t.Fatalf("subscriber.Subscribe(%s) calls = %d, want 2", physical, got)
		}
	}
	if got := f.params().Group; got != "orders-group" {
		t.Fatalf("SubscriberParams.Group = %q, want orders-group", got)
	}
	if got := f.params().Sarama; got == nil {
		t.Fatal("SubscriberParams.Sarama = nil, want built config")
	}

	msg := message.NewMessage("m1", []byte(`{"id":1}`))
	f.sub.channel("orders.v1") <- msg
	select {
	case d := <-deliveries:
		if d.Event.Topic != "orders" {
			t.Fatalf("delivery logical topic = %q, want orders", d.Event.Topic)
		}
		if string(d.Event.Payload) != `{"id":1}` {
			t.Fatalf("delivery payload = %q", d.Event.Payload)
		}
		d.Ack()
		select {
		case <-msg.Acked():
		case <-time.After(time.Second):
			t.Fatal("Ack not forwarded to the underlying message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery received")
	}
}

func TestErrorPaths(t *testing.T) {
	f := newBlackboxFactory()
	tr, err := wmkafka.NewTransport(ordersOptions(), wmkafka.WithClientFactory(f))
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	ctx := context.Background()

	if err := tr.Publish(ctx, "missing", &eventbus.RawEvent{}); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Publish(unconfigured) = %v, want not configured", err)
	}
	if _, err := tr.Subscribe(ctx, "missing", eventbus.SubscribeOptions{}); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Subscribe(unconfigured) = %v, want not configured", err)
	}

	// 只发布 / 只订阅的 topic。
	opts := wmkafka.Options{Topics: map[string]wmkafka.TopicOptions{
		"pub-only": {Brokers: []string{"127.0.0.1:9092"}, Topics: []string{"p1"},
			Producer: &wmkafka.ProducerOptions{Topic: "p1"}},
		"sub-only": {Brokers: []string{"127.0.0.1:9092"}, Topics: []string{"p2"},
			Consumer: &wmkafka.ConsumerOptions{GroupID: "g"}},
		"no-group": {Brokers: []string{"127.0.0.1:9092"}, Topics: []string{"p3"},
			Consumer: &wmkafka.ConsumerOptions{}},
	}}
	tr2, err := wmkafka.NewTransport(opts, wmkafka.WithClientFactory(f))
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if err := tr2.Publish(ctx, "sub-only", &eventbus.RawEvent{}); err == nil ||
		!strings.Contains(err.Error(), "no producer config") {
		t.Fatalf("Publish(no producer) = %v, want no producer config", err)
	}
	if _, err := tr2.Subscribe(ctx, "pub-only", eventbus.SubscribeOptions{}); err == nil ||
		!strings.Contains(err.Error(), "no consumer config") {
		t.Fatalf("Subscribe(no consumer) = %v, want no consumer config", err)
	}
	if _, err := tr2.Subscribe(ctx, "no-group", eventbus.SubscribeOptions{}); err == nil ||
		!strings.Contains(err.Error(), "consumer group required") {
		t.Fatalf("Subscribe(no group) = %v, want consumer group required", err)
	}
	if err := tr2.Publish(ctx, "pub-only", nil); err == nil ||
		!strings.Contains(err.Error(), "nil RawEvent") {
		t.Fatalf("Publish(nil event) = %v, want nil RawEvent", err)
	}
}
