package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/eventbus"
)

// captureLogs 收集 Warn 及以上日志（WK-06/WK-15 测试断言用）。
type captureLogs struct {
	mu   sync.Mutex
	logs []string
}

func newCaptureLogs() *captureLogs { return &captureLogs{} }

func (c *captureLogs) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (c *captureLogs) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value)
		return true
	})
	c.mu.Lock()
	c.logs = append(c.logs, sb.String())
	c.mu.Unlock()
	return nil
}
func (c *captureLogs) WithAttrs(attrs []slog.Attr) slog.Handler { return c }
func (c *captureLogs) WithGroup(name string) slog.Handler       { return c }

func (c *captureLogs) warnContains(substr string) bool {
	return c.warnCount(substr) > 0
}

// warnCount 统计包含 substr 的 Warn 条数（复审-3 去重断言用）。
func (c *captureLogs) warnCount(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.logs {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// TestInitRejectsInvalidSaramaConfigs 回归 WK-08：非法 sasl 机制/压缩/
// 初始 offset/TLS CA 路径/心跳超时失配等必须在 Init 即报错（离线
// cfg.Validate，不触网），不得延迟到首次 Publish/Subscribe 才暴露。
func TestInitRejectsInvalidSaramaConfigs(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"unsupported compression", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}, Topics: []string{"t"}, Producer: &ProducerOptions{Compression: "bogus"}},
		}}},
		{"invalid initial_offset", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}, Topics: []string{"t"}, Consumer: &ConsumerOptions{GroupID: "g", InitialOffset: "bogus"}},
		}}},
		{"unsupported sasl mechanism", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}, Topics: []string{"t"}, Producer: &ProducerOptions{},
				SASL: &SASLOptions{Enabled: true, User: "u", Password: "p", Mechanism: "GSSAPI"}},
		}}},
		{"sasl enabled without user", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}, Topics: []string{"t"}, Producer: &ProducerOptions{},
				SASL: &SASLOptions{Enabled: true, Password: "p"}},
		}}},
		{"heartbeat not less than session timeout", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}, Topics: []string{"t"}, Consumer: &ConsumerOptions{
				GroupID: "g", SessionTimeout: 6 * time.Second, HeartbeatInterval: 6 * time.Second}},
		}}},
		{"tls ca_file unreadable", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}, Topics: []string{"t"}, Producer: &ProducerOptions{},
				TLS: &TLSOptions{Enabled: true, CAFile: filepath.Join(t.TempDir(), "nope.pem")}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTransport(tt.opts, newFakePubSub())
			if err := tr.Init(newFakeApp()); err == nil {
				t.Fatal("expected Init error for invalid sarama config")
			}
		})
	}
}

// TestInitPrebuildsAndValidatesConfigs 回归 WK-08 正向面：合法配置（含
// SCRAM——NewFromConfig 默认注入生成器后 Validate 必须通过，不破坏默认
// 路径）Init 成功，且两侧 sarama 配置进入缓存供后续 Publish/Subscribe 复用。
func TestInitPrebuildsAndValidatesConfigs(t *testing.T) {
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"a": {
			Brokers:  []string{"b1"},
			Topics:   []string{"t1"},
			Producer: &ProducerOptions{Compression: "zstd", ClientID: "app"},
			Consumer: &ConsumerOptions{GroupID: "g", InitialOffset: "oldest", ClientID: "app"},
			SASL:     &SASLOptions{Enabled: true, User: "u", Password: "p", Mechanism: "SCRAM-SHA-256"},
		},
	}}, newFakePubSub())
	if err := tr.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tr.mu.Lock()
	pubCfg := tr.pubSaramaConfigs["b1"]
	subCfg := tr.subSaramaConfigs["b1"]
	tr.mu.Unlock()
	if pubCfg == nil || subCfg == nil {
		t.Fatalf("Init must prebuild both sarama configs (pub=%v sub=%v)", pubCfg != nil, subCfg != nil)
	}
	if pubCfg.Net.SASL.SCRAMClientGeneratorFunc == nil {
		t.Fatal("SCRAM generator must be injected by applyAuth before Validate")
	}
}

// TestProducerConfigConflictWarns 回归 WK-06：同集群（brokers 相同）多 topic
// 配置了不同 producer 参数时，客户端按 brokers 缓存、先构建者生效，后到
// 的差异配置必须 Warn 指出被忽略的 topic，而非静默吞掉。
func TestProducerConfigConflictWarns(t *testing.T) {
	lc := newCaptureLogs()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"a": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Producer: &ProducerOptions{ClientID: "a"}},
		"b": {Brokers: []string{"b1"}, Topics: []string{"t2"}, Producer: &ProducerOptions{ClientID: "b"}},
	}}, newFakePubSub())
	tr.logger = slog.New(lc)
	if err := tr.Publish(context.Background(), "a", &eventbus.RawEvent{ID: "1"}); err != nil {
		t.Fatalf("Publish a: %v", err)
	}
	if err := tr.Publish(context.Background(), "b", &eventbus.RawEvent{ID: "2"}); err != nil {
		t.Fatalf("Publish b: %v", err)
	}
	if !lc.warnContains("b") {
		t.Fatalf("expected warn naming ignored topic b, warns: %v", lc.logs)
	}
}

// TestConsumerConfigConflictWarns 回归 WK-06（订阅侧）：同 brokers 不同 topic
// 的 consumer 参数差异同样必须 Warn。
func TestConsumerConfigConflictWarns(t *testing.T) {
	lc := newCaptureLogs()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"a": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "ga", ClientID: "a"}},
		"b": {Brokers: []string{"b1"}, Topics: []string{"t2"}, Consumer: &ConsumerOptions{GroupID: "gb", ClientID: "b"}},
	}}, newFakePubSub())
	tr.logger = slog.New(lc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "a", eventbus.SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe a: %v", err)
	}
	if _, err := tr.Subscribe(ctx, "b", eventbus.SubscribeOptions{}); err != nil {
		t.Fatalf("Subscribe b: %v", err)
	}
	if !lc.warnContains("b") {
		t.Fatalf("expected warn naming ignored topic b, warns: %v", lc.logs)
	}
}

// TestSameConfigNoWarn 验证 WK-06 不误报：同 brokers 同配置的第二个 topic
// 命中缓存不产生 Warn。
func TestSameConfigNoWarn(t *testing.T) {
	lc := newCaptureLogs()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"a": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Producer: &ProducerOptions{ClientID: "same"}},
		"b": {Brokers: []string{"b1"}, Topics: []string{"t2"}, Producer: &ProducerOptions{ClientID: "same"}},
	}}, newFakePubSub())
	tr.logger = slog.New(lc)
	for _, topic := range []string{"a", "b"} {
		if err := tr.Publish(context.Background(), topic, &eventbus.RawEvent{ID: "1"}); err != nil {
			t.Fatalf("Publish %s: %v", topic, err)
		}
	}
	if lc.warnContains("ignored") {
		t.Fatalf("identical configs must not warn, warns: %v", lc.logs)
	}
}

// TestConfigConflictWarnedOnlyOnce 回归复审-3：配置指纹比对在每次
// Publish/Subscribe 都会命中，同一失配持续存在时（topic b 的配置始终被
// 忽略）重复 Publish 不得按消息速率刷 Warn——（side|brokers）维度只告警
// 首次。
func TestConfigConflictWarnedOnlyOnce(t *testing.T) {
	lc := newCaptureLogs()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"a": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Producer: &ProducerOptions{ClientID: "a"}},
		"b": {Brokers: []string{"b1"}, Topics: []string{"t2"}, Producer: &ProducerOptions{ClientID: "b"}},
	}}, newFakePubSub())
	tr.logger = slog.New(lc)
	// 先建立生效配置（a），再让失配方（b）连续命中缓存 5 次。
	if err := tr.Publish(context.Background(), "a", &eventbus.RawEvent{ID: "1"}); err != nil {
		t.Fatalf("Publish a: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := tr.Publish(context.Background(), "b", &eventbus.RawEvent{ID: "1"}); err != nil {
			t.Fatalf("Publish b #%d: %v", i+1, err)
		}
	}
	if got := lc.warnCount("ignored"); got != 1 {
		t.Fatalf("config-mismatch warns = %d, want exactly 1 (deduplicated per side+brokers)", got)
	}
}

// TestConfigFingerprintDereferencesNestedPointers 回归复审-6（单元）：
// 嵌套指针字段（如 ConsumerOptions.AutoCommitEnabled *bool）必须按指向值
// 参与指纹——同值不同指针实例同指纹；值不同指纹不同。
func TestConfigFingerprintDereferencesNestedPointers(t *testing.T) {
	c1 := &ConsumerOptions{GroupID: "g", AutoCommitEnabled: boolPtr(true)}
	c2 := &ConsumerOptions{GroupID: "g", AutoCommitEnabled: boolPtr(true)}
	if configFingerprint(c1, nil, nil) != configFingerprint(c2, nil, nil) {
		t.Fatal("same-valued configs with distinct *bool instances must share fingerprint")
	}
	if configFingerprint(c1, nil, nil) == configFingerprint(&ConsumerOptions{GroupID: "g", AutoCommitEnabled: boolPtr(false)}, nil, nil) {
		t.Fatal("different pointed-to values must produce different fingerprints")
	}
	// 顶层 nil 与零值结构语义不同（未配置 vs 显式零值），指纹必须区分。
	if configFingerprint(nil, nil, nil) == configFingerprint(&ConsumerOptions{}, nil, nil) {
		t.Fatal("nil options must be distinguishable from an empty struct")
	}
}

// TestSameValuePointerConfigNoWarn 回归复审-6（集成）：两个 topic 的配置
// 仅 *bool 字段使用不同指针实例（同值）时不告警；值真正不同（topic c）
// 仍必须告警——去指针不得吞掉真实差异。
func TestSameValuePointerConfigNoWarn(t *testing.T) {
	lc := newCaptureLogs()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"a": {Brokers: []string{"b1"}, Topics: []string{"t1"},
			Consumer: &ConsumerOptions{GroupID: "g", AutoCommitEnabled: boolPtr(false)}},
		"b": {Brokers: []string{"b1"}, Topics: []string{"t2"},
			Consumer: &ConsumerOptions{GroupID: "g", AutoCommitEnabled: boolPtr(false)}},
		"c": {Brokers: []string{"b1"}, Topics: []string{"t3"},
			Consumer: &ConsumerOptions{GroupID: "g", AutoCommitEnabled: boolPtr(true)}},
	}}, newFakePubSub())
	tr.logger = slog.New(lc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, topic := range []string{"a", "b", "c"} {
		if _, err := tr.Subscribe(ctx, topic, eventbus.SubscribeOptions{Group: "g"}); err != nil {
			t.Fatalf("Subscribe %s: %v", topic, err)
		}
	}
	if got := lc.warnCount("ignored"); got != 1 {
		t.Fatalf("warns = %d, want exactly 1 (only the real value difference on topic c), warns: %v", got, lc.logs)
	}
	if !lc.warnContains("topic=c") {
		t.Fatalf("the single warn must name topic c, warns: %v", lc.logs)
	}
}

// TestPublisherForRechecksStoppedUnderLock 回归 WK-07：stopped 快照检查与
// publisherFor 取锁之间存在窗口——Stop 在等 mu 期间完成并关闭客户端后，
// 持锁方必须复查 stopped 返回框架级错误，而非命中缓存报 sarama
// "client is closed"。
func TestPublisherForRechecksStoppedUnderLock(t *testing.T) {
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Producer: &ProducerOptions{}},
	}}, newFakePubSub())
	// 先建立缓存，命中路径才走"已关闭客户端"分支。
	if err := tr.Publish(context.Background(), "orders", &eventbus.RawEvent{ID: "1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	tr.mu.Lock()
	tr.stopped.Store(true) // 模拟 Stop 在持锁期间完成
	done := make(chan error, 1)
	go func() {
		_, err := tr.publisherFor([]string{"b1"}, "orders", &ProducerOptions{}, nil, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // 确保调用方已挂在 mu 上
	tr.mu.Unlock()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("publisherFor error = %v, want explicit stopped error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publisherFor did not return")
	}
}

// TestSubscriberForRechecksStoppedUnderLock 回归 WK-07（订阅侧）。
func TestSubscriberForRechecksStoppedUnderLock(t *testing.T) {
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "g"}},
	}}, newFakePubSub())

	tr.mu.Lock()
	tr.stopped.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := tr.subscriberFor([]string{"b1"}, "orders", "g", &ConsumerOptions{GroupID: "g"}, nil, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	tr.mu.Unlock()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("subscriberFor error = %v, want explicit stopped error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriberFor did not return")
	}
}

// TestSubscribeCtxDoneNacksInflightMessage 回归 WK-05：下游停读（返回的
// Delivery channel 无人消费）后订阅 ctx 取消，阻塞在裸发送上的
// mapDeliveries goroutine 必须退出，且 in-flight 消息被 Nack 交还 Transport
// （确认不静默丢失），输出 channel 随之关闭。
func TestSubscribeCtxDoneNacksInflightMessage(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "g1"}},
	}}, pub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// 故意不读 ch：模拟下游停读。

	deadline := time.Now().Add(5 * time.Second)
	var subCh chan *message.Message
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		n := len(pub.subChs["t1"])
		var first chan *message.Message
		if n > 0 {
			first = pub.subChs["t1"][0]
		}
		pub.mu.Unlock()
		if n > 0 {
			subCh = first
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if subCh == nil {
		t.Fatal("subscription was not established")
	}

	wm := message.NewMessage("inflight", []byte("x"))
	subCh <- wm // fanIn → mapDeliveries，最终阻塞在无人读取的 out<-
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-wm.Nacked():
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight message not Nacked on ctx done; goroutine leaked on bare channel send")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("delivery channel should be closed after ctx done")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delivery channel not closed after ctx done")
	}
}

// TestLogMessagesCtxDoneNacks 回归 WK-05（logMessages 直接单测）：
// ctx 取消时阻塞在发送处的 goroutine 必须 Nack in-flight 消息并退出。
func TestLogMessagesCtxDoneNacks(t *testing.T) {
	tr := newTestTransport(Options{}, newFakePubSub())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan *message.Message)
	out := tr.logMessages(ctx, "t1", in)

	wm := message.NewMessage("m1", []byte("x"))
	in <- wm // logMessages 收到并阻塞在 out<-（无人读取）
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-wm.Nacked():
	case <-time.After(3 * time.Second):
		t.Fatal("logMessages did not Nack in-flight message on ctx done")
	}
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("logMessages output should be closed after ctx done")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("logMessages output not closed after ctx done")
	}
}

// TestFanInCtxDoneExitsAndNacks 回归 WK-05（fanIn 直接单测）：上游 channel
// 未关闭（fake/异常路径）时 ctx 取消也必须让 worker 退出。
func TestFanInCtxDoneExitsAndNacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan *message.Message)
	out := fanIn(ctx, []<-chan *message.Message{in}, func() {})

	wm := message.NewMessage("m1", []byte("x"))
	in <- wm // worker 收到并阻塞在 out<-（无人读取）
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-wm.Nacked():
	case <-time.After(3 * time.Second):
		t.Fatal("fanIn did not Nack in-flight message on ctx done")
	}
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("fanIn output should be closed after ctx done")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fanIn output not closed after ctx done")
	}
}

// TestSubscribeInstancesClampedToLimit 回归 WK-15：instances=1000 会创建
// 1000 个消费组连接，必须钳制到 maxConsumerInstances 并告警。
func TestSubscribeInstancesClampedToLimit(t *testing.T) {
	lc := newCaptureLogs()
	pub := newFakePubSub()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "g1"}},
	}}, pub)
	tr.logger = slog.New(lc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "orders", eventbus.SubscribeOptions{Instances: 1000}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pub.subscribeCount("t1") >= maxConsumerInstances {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := pub.subscribeCount("t1"); got != maxConsumerInstances {
		t.Fatalf("instances = %d, want clamped to %d", got, maxConsumerInstances)
	}
	if !lc.warnContains("clamped") {
		t.Fatalf("expected clamp warning, warns: %v", lc.logs)
	}
}
