package kafka

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

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// boolPtr 返回指向 b 的指针（测试辅助）。
func boolPtr(b bool) *bool { return &b }

// pubSubClient 是 client seam 的最小接口，供 fake（fakePubSub / failingPubSub）实现。
type pubSubClient interface {
	message.Publisher
	message.Subscriber
}

// fakePubSub 是 client seam 的 fake：记录 Publish/Subscribe 调用。
type fakePubSub struct {
	mu        sync.Mutex
	published []string
	msgs      []*message.Message
	subChs    map[string][]chan *message.Message // key: topic
	closed    atomic.Int32
}

func newFakePubSub() *fakePubSub {
	return &fakePubSub{subChs: map[string][]chan *message.Message{}}
}

func (f *fakePubSub) Publish(topic string, msgs ...*message.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, topic)
	f.msgs = append(f.msgs, msgs...)
	return nil
}

func (f *fakePubSub) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	ch := make(chan *message.Message)
	f.mu.Lock()
	f.subChs[topic] = append(f.subChs[topic], ch)
	f.mu.Unlock()
	return ch, nil
}

func (f *fakePubSub) Close() error {
	f.closed.Add(1)
	return nil
}

func (f *fakePubSub) publishTopics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.published...)
}

func (f *fakePubSub) publishedMsgs() []*message.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*message.Message(nil), f.msgs...)
}

func (f *fakePubSub) subscribeCount(topic string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subChs[topic])
}

// newTestTransport 构造注入 fake client seam 的 Transport。
func newTestTransport(opts Options, pub pubSubClient) *Transport {
	t := &Transport{
		opts:             opts,
		logger:           slog.Default(),
		publishers:       map[string]message.Publisher{},
		subscribers:      map[string]message.Subscriber{},
		pubSaramaConfigs: map[string]*sarama.Config{},
		subSaramaConfigs: map[string]*sarama.Config{},
		newPublisher: func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error) {
			return pub, nil
		},
		newSubscriber: func(p subscriberParams, logger watermill.LoggerAdapter) (message.Subscriber, error) {
			return pub, nil
		},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t
}

// captureFactory 是记录型 fake seam：把工厂收到的 sarama 配置与订阅参数
// 存入 atomic.Value，供配置映射断言读取。
type captureFactory struct {
	pub        *fakePubSub
	lastCfg    atomic.Value // *sarama.Config
	lastParams atomic.Value // subscriberParams
}

// newCapturingTransport 构造注入记录型 fake seam 的 Transport。
func newCapturingTransport(opts Options, cap *captureFactory) *Transport {
	t := &Transport{
		opts:             opts,
		logger:           slog.Default(),
		publishers:       map[string]message.Publisher{},
		subscribers:      map[string]message.Subscriber{},
		pubSaramaConfigs: map[string]*sarama.Config{},
		subSaramaConfigs: map[string]*sarama.Config{},
		newPublisher: func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error) {
			cap.lastCfg.Store(cfg)
			return cap.pub, nil
		},
		newSubscriber: func(p subscriberParams, logger watermill.LoggerAdapter) (message.Subscriber, error) {
			cap.lastCfg.Store(p.sarama)
			cap.lastParams.Store(p)
			return cap.pub, nil
		},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t
}

func TestOptionsFromConfig(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
kafka:
  orders:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_orders, topic_orders_v2]
    consumer:
      group_id: orders-group
      instances: 3
      commit_interval: 1s
      auto_commit_enabled: false
      initial_offset: oldest
      log_message: true
      nack_resend_sleep: 500ms
      reconnect_retry_sleep: 3s
      session_timeout: 45s
      heartbeat_interval: 5s
      fetch_min_bytes: 1024
      fetch_max_bytes: 5242880
      fetch_max_wait: 600ms
      client_id: orders-app
    producer:
      topic: topic_orders_v2
      log_message: true
      batch_size: 100
      required_acks: -1
      retry_max: 8
      timeout: 15s
      flush_bytes: 4096
      flush_frequency: 200ms
      compression: gzip
      client_id: orders-app
  payments:
    brokers: ["10.0.0.2:9092"]
    topics: [payments_topic]
    consumer:
      group_id: payments-group
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	var opts Options
	if err := v.UnmarshalKey("kafka", &opts); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	if len(opts.Topics) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(opts.Topics))
	}
	orders, ok := opts.Topics["orders"]
	if !ok {
		t.Fatalf("missing orders: %+v", opts.Topics)
	}
	if len(orders.Brokers) != 1 || orders.Brokers[0] != "127.0.0.1:19092" {
		t.Fatalf("bad brokers: %+v", orders.Brokers)
	}
	if len(orders.Topics) != 2 || orders.Topics[1] != "topic_orders_v2" {
		t.Fatalf("bad topics: %+v", orders.Topics)
	}
	if orders.Consumer == nil || orders.Consumer.GroupID != "orders-group" || orders.Consumer.Instances != 3 {
		t.Fatalf("bad consumer: %+v", orders.Consumer)
	}
	if orders.Consumer.CommitInterval != time.Second {
		t.Fatalf("bad commit interval: %v", orders.Consumer.CommitInterval)
	}
	if orders.Consumer.InitialOffset != "oldest" {
		t.Fatalf("bad initial offset: %q", orders.Consumer.InitialOffset)
	}
	if orders.Consumer.AutoCommitEnabled == nil || *orders.Consumer.AutoCommitEnabled {
		t.Fatalf("bad auto commit enabled: %v", orders.Consumer.AutoCommitEnabled)
	}
	if orders.Producer == nil || orders.Producer.Topic != "topic_orders_v2" || !orders.Producer.LogMessage {
		t.Fatalf("bad producer: %+v", orders.Producer)
	}
	if orders.Producer.BatchSize != 100 {
		t.Fatalf("bad batch size: %d", orders.Producer.BatchSize)
	}
	// 新增常用配置项解析。
	if orders.Consumer.NackResendSleep != 500*time.Millisecond {
		t.Fatalf("bad nack resend sleep: %v", orders.Consumer.NackResendSleep)
	}
	if orders.Consumer.ReconnectRetrySleep != 3*time.Second {
		t.Fatalf("bad reconnect retry sleep: %v", orders.Consumer.ReconnectRetrySleep)
	}
	if orders.Consumer.SessionTimeout != 45*time.Second {
		t.Fatalf("bad session timeout: %v", orders.Consumer.SessionTimeout)
	}
	if orders.Consumer.HeartbeatInterval != 5*time.Second {
		t.Fatalf("bad heartbeat interval: %v", orders.Consumer.HeartbeatInterval)
	}
	if orders.Consumer.FetchMinBytes != 1024 {
		t.Fatalf("bad fetch min bytes: %d", orders.Consumer.FetchMinBytes)
	}
	if orders.Consumer.FetchMaxBytes != 5*1024*1024 {
		t.Fatalf("bad fetch max bytes: %d", orders.Consumer.FetchMaxBytes)
	}
	if orders.Consumer.FetchMaxWait != 600*time.Millisecond {
		t.Fatalf("bad fetch max wait: %v", orders.Consumer.FetchMaxWait)
	}
	if orders.Consumer.ClientID != "orders-app" {
		t.Fatalf("bad consumer client id: %q", orders.Consumer.ClientID)
	}
	if orders.Producer.RequiredAcks != -1 {
		t.Fatalf("bad required acks: %d", orders.Producer.RequiredAcks)
	}
	if orders.Producer.RetryMax != 8 {
		t.Fatalf("bad retry max: %d", orders.Producer.RetryMax)
	}
	if orders.Producer.Timeout != 15*time.Second {
		t.Fatalf("bad producer timeout: %v", orders.Producer.Timeout)
	}
	if orders.Producer.FlushBytes != 4096 {
		t.Fatalf("bad flush bytes: %d", orders.Producer.FlushBytes)
	}
	if orders.Producer.FlushFrequency != 200*time.Millisecond {
		t.Fatalf("bad flush frequency: %v", orders.Producer.FlushFrequency)
	}
	if orders.Producer.Compression != "gzip" {
		t.Fatalf("bad compression: %q", orders.Producer.Compression)
	}
	if orders.Producer.ClientID != "orders-app" {
		t.Fatalf("bad producer client id: %q", orders.Producer.ClientID)
	}
}

func TestBuildSaramaConfigMappings(t *testing.T) {
	// 消费侧与发布侧各用独立集群（不同 brokers），保证每个 *sarama.Config
	// 都是首次构建、完整带上各自的映射。
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers: []string{"b1"},
				Topics:  []string{"t1"},
				Consumer: &ConsumerOptions{
					GroupID:             "g1",
					CommitInterval:      2 * time.Second,
					AutoCommitEnabled:   boolPtr(false),
					InitialOffset:       "oldest",
					NackResendSleep:     500 * time.Millisecond,
					ReconnectRetrySleep: 3 * time.Second,
					SessionTimeout:      45 * time.Second,
					HeartbeatInterval:   5 * time.Second,
					FetchMinBytes:       1024,
					FetchMaxBytes:       5 * 1024 * 1024,
					FetchMaxWait:        600 * time.Millisecond,
					ClientID:            "orders-app",
				},
			},
			"notify": {
				Brokers: []string{"b2"},
				Topics:  []string{"t2"},
				Producer: &ProducerOptions{
					BatchSize:      100,
					RequiredAcks:   -1,
					RetryMax:       8,
					Timeout:        15 * time.Second,
					FlushBytes:     4096,
					FlushFrequency: 200 * time.Millisecond,
					Compression:    "gzip",
					ClientID:       "notify-app",
				},
			},
		},
	}, cap)

	// 发布侧：Publish 触发 publisherFor → buildSaramaConfig(brokers, nil, producer)。
	if err := tr.Publish(context.Background(), "notify", &eventbus.RawEvent{ID: "id"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	producerCfg := cap.lastCfg.Load().(*sarama.Config)
	producerChecks := []struct {
		name string
		got  any
		want any
	}{
		{"flush messages (batch_size)", producerCfg.Producer.Flush.Messages, 100},
		{"required acks", producerCfg.Producer.RequiredAcks, sarama.WaitForAll},
		{"retry max", producerCfg.Producer.Retry.Max, 8},
		{"timeout", producerCfg.Producer.Timeout, 15 * time.Second},
		{"flush bytes", producerCfg.Producer.Flush.Bytes, 4096},
		{"flush frequency", producerCfg.Producer.Flush.Frequency, 200 * time.Millisecond},
		{"compression", producerCfg.Producer.Compression, sarama.CompressionGZIP},
		{"client id", producerCfg.ClientID, "notify-app"},
	}
	for _, c := range producerChecks {
		if c.got != c.want {
			t.Fatalf("producer %s: got %v, want %v", c.name, c.got, c.want)
		}
	}

	// 消费侧：Subscribe 触发 subscriberFor → buildSaramaConfig(brokers, consumer, nil)。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	consumerCfg := cap.lastCfg.Load().(*sarama.Config)
	consumerChecks := []struct {
		name string
		got  any
		want any
	}{
		{"auto commit interval", consumerCfg.Consumer.Offsets.AutoCommit.Interval, 2 * time.Second},
		{"auto commit enabled", consumerCfg.Consumer.Offsets.AutoCommit.Enable, false},
		{"initial offset", consumerCfg.Consumer.Offsets.Initial, sarama.OffsetOldest},
		{"session timeout", consumerCfg.Consumer.Group.Session.Timeout, 45 * time.Second},
		{"heartbeat interval", consumerCfg.Consumer.Group.Heartbeat.Interval, 5 * time.Second},
		{"fetch min bytes", consumerCfg.Consumer.Fetch.Min, int32(1024)},
		{"fetch max bytes", consumerCfg.Consumer.Fetch.Max, int32(5 * 1024 * 1024)},
		{"fetch max wait", consumerCfg.Consumer.MaxWaitTime, 600 * time.Millisecond},
		{"client id", consumerCfg.ClientID, "orders-app"},
	}
	for _, c := range consumerChecks {
		if c.got != c.want {
			t.Fatalf("consumer %s: got %v, want %v", c.name, c.got, c.want)
		}
	}

	// watermill 层参数：NackResendSleep / ReconnectRetrySleep 随 subscriberParams 传递。
	params := cap.lastParams.Load().(subscriberParams)
	if params.group != "g1" {
		t.Fatalf("group: got %q, want g1", params.group)
	}
	if params.nackResendSleep != 500*time.Millisecond {
		t.Fatalf("nack resend sleep: got %v, want 500ms", params.nackResendSleep)
	}
	if params.reconnectRetrySleep != 3*time.Second {
		t.Fatalf("reconnect retry sleep: got %v, want 3s", params.reconnectRetrySleep)
	}
}

func TestCompressionInvalid(t *testing.T) {
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Producer: &ProducerOptions{Compression: "bogus"},
			},
		},
	}, cap)
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id"}); err == nil {
		t.Fatal("expected Publish error for invalid compression")
	}
}

func TestInitialOffsetInvalid(t *testing.T) {
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers: []string{"b1"},
				Topics:  []string{"t1"},
				Consumer: &ConsumerOptions{
					GroupID:       "g1",
					InitialOffset: "bogus",
				},
			},
		},
	}, cap)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{}); err == nil {
		t.Fatal("expected Subscribe error for invalid initial_offset")
	}
}

func TestTransportTopics(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders":   {Brokers: []string{"b"}, Topics: []string{"t1"}},
			"payments": {Brokers: []string{"b"}, Topics: []string{"t2"}},
		},
	}, pub)
	got := tr.Topics()
	if len(got) != 2 {
		t.Fatalf("expected 2 topics, got %v", got)
	}
}

func TestTransportPublishResolvesPhysicalTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1", "t2"},
				Producer: &ProducerOptions{Topic: "t2"},
			},
		},
	}, pub)

	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := pub.publishTopics(); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("expected publish to t2, got %v", got)
	}
}

func TestTransportPublishDefaultPhysicalTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b1"}, Topics: []string{"t1", "t2"}, Producer: &ProducerOptions{}},
		},
	}, pub)

	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := pub.publishTopics(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("expected publish to t1 (Topics[0]), got %v", got)
	}
}

func TestTransportPublishNoProducerConfig(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "g"}},
		},
	}, pub)

	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id"}); err == nil {
		t.Fatal("expected Publish error without producer config")
	}
}

func TestTransportPublishUnknownTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{}}, pub)
	if err := tr.Publish(context.Background(), "nope", &eventbus.RawEvent{ID: "id"}); err == nil {
		t.Fatal("expected Publish error for unknown topic")
	}
}

func TestTransportSubscribeExpansion(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1", "t2"},
				Consumer: &ConsumerOptions{GroupID: "g1", Instances: 3},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// 2 物理 topic × 3 实例 = 6 个底层订阅。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pub.subscribeCount("t1") == 3 && pub.subscribeCount("t2") == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pub.subscribeCount("t1") != 3 || pub.subscribeCount("t2") != 3 {
		t.Fatalf("expected 3 subscriptions per topic, got t1=%d t2=%d",
			pub.subscribeCount("t1"), pub.subscribeCount("t2"))
	}

	// fan-in：来自两个物理 topic 的消息都能收到。
	sent := message.NewMessage("id-1", []byte("x"))
	ch1 := pub.subChs["t1"][0]
	go func() { ch1 <- sent }()
	select {
	case got := <-ch:
		if got.Event == nil || got.Event.ID != "id-1" {
			t.Fatalf("unexpected message: %+v", got)
		}
		if got.Event.Topic != "orders" {
			t.Fatalf("logical topic = %q, want orders", got.Event.Topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive fan-in message")
	}
}

func TestTransportSubscribeGroupOverride(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Consumer: &ConsumerOptions{GroupID: "config-group"},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{Group: "code-group"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	// fake 不区分组（Subscribe 只按 topic 记录），此处仅验证不报错。
}

func TestTransportSubscribeMissingGroup(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{}); err == nil {
		t.Fatal("expected Subscribe error when group is missing")
	}
}

func TestTransportSubscribeUnknownTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{}}, pub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "nope", eventbus.SubscribeOptions{}); err == nil {
		t.Fatal("expected Subscribe error for unknown topic")
	}
}

func TestTransportInitValidation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"missing brokers", Options{Topics: map[string]TopicOptions{
			"a": {Topics: []string{"t"}},
		}}},
		{"missing topics", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := newFakePubSub()
			tr := newTestTransport(tt.opts, pub)
			if err := tr.Init(newFakeApp()); err == nil {
				t.Fatal("expected Init error")
			}
		})
	}
}

func TestTransportLifecycle(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b"}, Topics: []string{"t"}, Consumer: &ConsumerOptions{GroupID: "g"}},
		},
	}, pub)
	if err := tr.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := tr.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error before Start")
	}

	startCtx, startCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Start(startCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tr.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tr.CheckHealth(); err != nil {
		t.Fatalf("transport unhealthy after Start: %v", err)
	}

	// 建立客户端：Subscribe 触发 subscriber 创建（fake），Stop 时关闭。
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	if _, err := tr.Subscribe(subCtx, "orders", eventbus.SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	_ = tr.Stop(context.Background())
	startCancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	if pub.closed.Load() < 1 {
		t.Fatal("expected client Close on Stop")
	}
	if err := tr.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error after Stop")
	}
}

// TestTransportStartRespectsCtx 回归 P1-3：Start 必须阻塞在**传入的** ctx
// 上——直接取消 Start 的 ctx（不调用 Stop）即应返回，与 schedule/telemetry
// 的服务契约一致，不能只依赖 Stop 取消的内部 ctx。
func TestTransportStartRespectsCtx(t *testing.T) {
	tr := newTestTransport(Options{}, newFakePubSub())
	startCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Start(startCtx) }()

	// 不调用 Stop，仅取消 Start 的 ctx。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after its ctx was cancelled")
	}
}

// TestTransportStopBeforeStart 回归 P1-5：Stop 必须先于 Start 调用被容忍
// （Init 成功但 Start 未执行的失败清理路径）——不 panic、不挂死，
// 且 Stop 后健康检查保持失败。
func TestTransportStopBeforeStart(t *testing.T) {
	tests := []struct {
		name string
		init bool // 是否先调用 Init
	}{
		{name: "before Init", init: false},
		{name: "after Init before Start", init: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTransport(Options{
				Topics: map[string]TopicOptions{
					"orders": {Brokers: []string{"b"}, Topics: []string{"t"}, Consumer: &ConsumerOptions{GroupID: "g"}},
				},
			}, newFakePubSub())
			if tt.init {
				if err := tr.Init(newFakeApp()); err != nil {
					t.Fatalf("Init: %v", err)
				}
			}
			_ = tr.Stop(context.Background())
			if err := tr.CheckHealth(); err == nil {
				t.Fatal("expected CheckHealth error after Stop-before-Start")
			}
		})
	}
}

// TestTransportPublishAfterStop 回归 P2-4：Stop 后 Publish 必须返回框架级
// 错误，不得命中缓存的已关闭 publisher（此前报 sarama "client is closed"）。
func TestTransportPublishAfterStop(t *testing.T) {
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"orders_v1"}, Producer: &ProducerOptions{}},
	}}, newFakePubSub())
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id"}); err != nil {
		t.Fatalf("Publish before Stop: %v", err)
	}
	_ = tr.Stop(context.Background())
	err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id2"})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Publish after Stop error = %v, want explicit stopped error", err)
	}
}

// TestTransportSubscribeAfterStop 回归二轮复审项 2：Stop 后 Subscribe 必须
// 返回与 Publish 同款框架级错误，不得命中缓存的已关闭 subscriber。
func TestTransportSubscribeAfterStop(t *testing.T) {
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "g1"}},
	}}, newFakePubSub())
	if _, err := tr.Subscribe(context.Background(), "orders", eventbus.SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe before Stop: %v", err)
	}
	_ = tr.Stop(context.Background())
	_, err := tr.Subscribe(context.Background(), "orders", eventbus.SubscribeOptions{})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Subscribe after Stop error = %v, want explicit stopped error", err)
	}
}

func TestTransportPublishLogMessageWithoutInit(t *testing.T) {
	// 未 Init（t.app == nil）即 Publish 且 log_message=true 不得 panic。
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Producer: &ProducerOptions{LogMessage: true},
			},
		},
	}, pub)
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id", Payload: []byte("hello")}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := pub.publishTopics(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("expected publish to t1, got %v", got)
	}
}

// failingPubSub 是 client seam 的 fake：第 failOn 次 Subscribe 返回错误，
// 其余订阅的 channel 在 ctx 取消时关闭（模拟 watermill 订阅语义）。
type failingPubSub struct {
	failOn int32
	subs   int32
	mu     sync.Mutex
	chans  []chan *message.Message
}

func newFailingPubSub(failOn int) *failingPubSub {
	return &failingPubSub{failOn: int32(failOn)}
}

func (f *failingPubSub) Publish(topic string, msgs ...*message.Message) error { return nil }

func (f *failingPubSub) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	if atomic.AddInt32(&f.subs, 1) >= f.failOn {
		return nil, errors.New("subscribe failed")
	}
	ch := make(chan *message.Message)
	f.mu.Lock()
	f.chans = append(f.chans, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (f *failingPubSub) Close() error { return nil }

func (f *failingPubSub) channels() []chan *message.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chan *message.Message(nil), f.chans...)
}

func TestTransportSubscribeExpansionFailureCleansUp(t *testing.T) {
	// 展开中途第 3 个子订阅失败：已建立的子订阅 channel 必须随
	// 派生 ctx 取消而关闭，不得泄漏。
	pub := newFailingPubSub(3)
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1", "t2", "t3"},
				Consumer: &ConsumerOptions{GroupID: "g1"},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{}); err == nil {
		t.Fatal("expected Subscribe error when the 3rd underlying subscribe fails")
	}

	chans := pub.channels()
	if len(chans) != 2 {
		t.Fatalf("expected 2 established subscriptions before failure, got %d", len(chans))
	}
	for i, ch := range chans {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("subscription channel %d still open after cleanup", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscription channel %d not closed after cleanup", i)
		}
	}
}

func TestTransportSubscribeRawEventWire(t *testing.T) {
	// Transport 层：Subscribe 将 watermill 消息解码为 RawEvent（含 wire 元数据）。
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Consumer: &ConsumerOptions{GroupID: "g1"},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var subCh chan *message.Message
	for time.Now().Before(deadline) {
		if pub.subscribeCount("t1") == 1 {
			subCh = pub.subChs["t1"][0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if subCh == nil {
		t.Fatal("subscription was not established")
	}

	wm := message.NewMessage("id-1", []byte("payload"))
	wm.Metadata.Set(eventbus.MetaMessageKey, "k1")
	wm.Metadata.Set(eventbus.MetaLogicalTopic, "orders")
	subCh <- wm

	select {
	case got := <-ch:
		if got.Event == nil || got.Event.ID != "id-1" || string(got.Event.Payload) != "payload" || got.Event.Key != "k1" {
			t.Fatalf("unexpected RawEvent: %+v", got.Event)
		}
		if got.Event.Topic != "orders" {
			t.Fatalf("topic = %q, want orders", got.Event.Topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive RawEvent")
	}
}

func TestTransportSubscribeDeliveryAckNack(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Consumer: &ConsumerOptions{GroupID: "g1"},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var subCh chan *message.Message
	for time.Now().Before(deadline) {
		if pub.subscribeCount("t1") == 1 {
			subCh = pub.subChs["t1"][0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if subCh == nil {
		t.Fatal("subscription was not established")
	}

	wmAck := message.NewMessage("ack-id", []byte("a"))
	subCh <- wmAck
	select {
	case d := <-ch:
		d.AckOnce()
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery for ack")
	}
	select {
	case <-wmAck.Acked():
	case <-time.After(2 * time.Second):
		t.Fatal("Delivery.Ack did not Ack underlying watermill message")
	}

	wmNack := message.NewMessage("nack-id", []byte("n"))
	subCh <- wmNack
	select {
	case d := <-ch:
		d.NackOnce()
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery for nack")
	}
	select {
	case <-wmNack.Nacked():
	case <-time.After(2 * time.Second):
		t.Fatal("Delivery.Nack did not Nack underlying watermill message")
	}
}

func TestTransportCloseDelegatesStop(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b"}, Topics: []string{"t"}, Producer: &ProducerOptions{}},
		},
	}, pub)
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pub.closed.Load() < 1 {
		t.Fatal("expected publisher Close via Transport.Close")
	}
	err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "id2"})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Publish after Close error = %v, want stopped", err)
	}
}

// --- fakeApp：最小 lynx.AppContext（服务 Init 只依赖 AppContext，无需实现完整 App） ---

type fakeApp struct{}

func newFakeApp() *fakeApp { return &fakeApp{} }

func (a *fakeApp) Context() context.Context       { return context.Background() }
func (a *fakeApp) Config() lynx.Config            { return lynx.NewViperConfig(viper.New()) }
func (a *fakeApp) Bus() eventbus.Bus                   { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (a *fakeApp) HealthCheckers() []lynx.Checker { return nil }
func (a *fakeApp) Close()                         {}
func (a *fakeApp) Logger(_ ...any) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var _ lynx.AppContext = (*fakeApp)(nil)
