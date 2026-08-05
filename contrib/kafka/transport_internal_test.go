package kafka

import (
	"context"
	"testing"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/contrib/pubsub"
)

// TestPublisherConfigReturnSuccesses 回归 C1：SyncProducer 必需配置。
// watermill-kafka 仅在 OverwriteSaramaConfig 为 nil 时应用默认配置
// （Return.Successes=true），此处显式设置，否则真实发布必然失败。
func TestPublisherConfigReturnSuccesses(t *testing.T) {
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{Topics: map[string]TopicOptions{
		"orders": {Brokers: []string{"b1:9092"}, Topics: []string{"orders_v1"}, Producer: &ProducerOptions{}},
	}}, cap)
	if err := tr.Publish(context.Background(), "orders", message.NewMessage("id", nil)); err != nil {
		t.Fatal(err)
	}
	cfg, ok := cap.lastCfg.Load().(*sarama.Config)
	if !ok || cfg == nil {
		t.Fatal("publisher factory did not receive a sarama config")
	}
	if !cfg.Producer.Return.Successes {
		t.Fatal("Producer.Return.Successes must be true for SyncProducer")
	}
	if !cfg.Producer.Return.Errors {
		t.Fatal("Producer.Return.Errors must be true for SyncProducer")
	}
}

// TestRealPublisherFactoryNoMissingMarshaler 回归 C1 第一道屏障：
// 真实 watermill-kafka 工厂在补上 Marshaler 后不再报 "missing marshaler"。
// 无需真实 broker：sarama 配置校验先于网络连接。
func TestRealPublisherFactoryNoMissingMarshaler(t *testing.T) {
	cfg := sarama.NewConfig()
	cfg.Producer.Return.Successes = true
	cfg.Producer.Return.Errors = true
	p, err := watermillkafka.NewPublisher(watermillkafka.PublisherConfig{
		Brokers:               []string{"127.0.0.1:19092"},
		OverwriteSaramaConfig: cfg,
		Marshaler:             watermillkafka.DefaultMarshaler{},
	}, watermill.NewStdLogger(false, false))
	if err != nil {
		t.Fatalf("real publisher factory failed: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSASLTLSConfigMapping 验证 SASL/TLS 认证配置映射。
func TestSASLTLSConfigMapping(t *testing.T) {
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{Topics: map[string]TopicOptions{
		"orders": {
			Brokers:  []string{"b1:9092"},
			Topics:   []string{"orders_v1"},
			Producer: &ProducerOptions{},
			SASL: &SASLOptions{
				Enabled: true, User: "u", Password: "p", Mechanism: "SCRAM-SHA-256",
			},
			TLS: &TLSOptions{Enabled: true, InsecureSkipVerify: true, ServerName: "kafka.internal"},
		},
	}}, cap)
	if err := tr.Publish(context.Background(), "orders", message.NewMessage("id", nil)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := cap.lastCfg.Load().(*sarama.Config)
	if !cfg.Net.SASL.Enable || cfg.Net.SASL.User != "u" || cfg.Net.SASL.Password != "p" {
		t.Fatalf("SASL mapping wrong: %+v", cfg.Net.SASL)
	}
	if cfg.Net.SASL.Mechanism != sarama.SASLTypeSCRAMSHA256 {
		t.Fatalf("mechanism = %v, want SCRAM-SHA-256", cfg.Net.SASL.Mechanism)
	}
	if !cfg.Net.TLS.Enable || cfg.Net.TLS.Config == nil ||
		!cfg.Net.TLS.Config.InsecureSkipVerify || cfg.Net.TLS.Config.ServerName != "kafka.internal" {
		t.Fatalf("TLS mapping wrong: %+v", cfg.Net.TLS)
	}
}

// TestSASLInvalidMechanism 验证非法机制返回错误。
func TestSASLInvalidMechanism(t *testing.T) {
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{Topics: map[string]TopicOptions{
		"orders": {
			Brokers:  []string{"b1:9092"},
			Producer: &ProducerOptions{},
			SASL:     &SASLOptions{Enabled: true, User: "u", Password: "p", Mechanism: "GSSAPI"},
		},
	}}, cap)
	if err := tr.Publish(context.Background(), "orders", message.NewMessage("id", nil)); err == nil {
		t.Fatal("expected error for unsupported sasl mechanism")
	}
}

// TestPerSideConfigCache 回归 M6：consumer/producer 参数正交，按侧独立
// 缓存，不再静默丢失另一侧配置。
func TestPerSideConfigCache(t *testing.T) {
	cap := &captureFactory{pub: newFakePubSub()}
	tr := newCapturingTransport(Options{Topics: map[string]TopicOptions{
		"orders": {
			Brokers:  []string{"b1:9092"},
			Topics:   []string{"orders_v1"},
			Consumer: &ConsumerOptions{GroupID: "g", FetchMinBytes: 4096, InitialOffset: "oldest", ClientID: "consumer-id"},
			Producer: &ProducerOptions{BatchSize: 100, ClientID: "producer-id"},
		},
	}}, cap)
	if err := tr.Publish(context.Background(), "orders", message.NewMessage("id", nil)); err != nil {
		t.Fatal(err)
	}
	pubCfg, _ := cap.lastCfg.Load().(*sarama.Config)
	if pubCfg.Producer.Flush.Messages != 100 {
		t.Fatalf("producer batch size not applied: %d", pubCfg.Producer.Flush.Messages)
	}
	if pubCfg.ClientID != "producer-id" {
		t.Fatalf("producer client id = %q, want producer-id", pubCfg.ClientID)
	}

	if _, err := tr.Subscribe(context.Background(), "orders", pubsub.SubscriptionOptions{Group: "g"}); err != nil {
		t.Fatal(err)
	}
	subCfg, _ := cap.lastCfg.Load().(*sarama.Config)
	if subCfg.Consumer.Fetch.Min != 4096 {
		t.Fatalf("consumer fetch min not applied: %d", subCfg.Consumer.Fetch.Min)
	}
	if subCfg.Consumer.Offsets.Initial != sarama.OffsetOldest {
		t.Fatalf("consumer initial offset not applied: %v", subCfg.Consumer.Offsets.Initial)
	}
	if subCfg.ClientID != "consumer-id" {
		t.Fatalf("consumer client id = %q, want consumer-id", subCfg.ClientID)
	}
	// 两侧互不污染：producer 侧保持 sarama 默认（newest / flush 0）。
	if pubCfg.Consumer.Offsets.Initial != sarama.OffsetNewest {
		t.Fatalf("producer side unexpectedly has consumer offset config: %v", pubCfg.Consumer.Offsets.Initial)
	}
	if subCfg.Producer.Flush.Messages != 0 {
		t.Fatalf("consumer side unexpectedly has producer params: %d", subCfg.Producer.Flush.Messages)
	}
}
