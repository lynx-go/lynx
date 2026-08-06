package kafka

import (
	"context"
	"fmt"
	"testing"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/xdg-go/scram"
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

// TestSCRAMConfigPassesSaramaValidate 回归 P0-2：SCRAM 配置必须能通过
// sarama.Config.Validate() 的真实校验路径（此前只设置 Mechanism 而不设置
// SCRAMClientGeneratorFunc，Validate() 报 "A SCRAMClientGeneratorFunc
// function must be provided"，Publish 先于网络连接即失败）。
// 本测试直接调用真实配置构建与 sarama 校验，不经 fake seam。
func TestSCRAMConfigPassesSaramaValidate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mechanism string
		want      sarama.SASLMechanism
	}{
		{"SHA-256", "SCRAM-SHA-256", sarama.SASLTypeSCRAMSHA256},
		{"SHA-512", "SCRAM-SHA-512", sarama.SASLTypeSCRAMSHA512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := NewTransport(Options{})
			if err != nil {
				t.Fatalf("NewTransport: %v", err)
			}
			cfg, err := tr.buildPublisherConfig(
				[]string{"b1:9092"},
				&ProducerOptions{},
				&SASLOptions{Enabled: true, User: "u", Password: "p", Mechanism: tc.mechanism},
				nil,
			)
			if err != nil {
				t.Fatalf("buildPublisherConfig: %v", err)
			}
			if cfg.Net.SASL.Mechanism != tc.want {
				t.Fatalf("mechanism = %v, want %v", cfg.Net.SASL.Mechanism, tc.want)
			}
			if cfg.Net.SASL.SCRAMClientGeneratorFunc == nil {
				t.Fatal("SCRAMClientGeneratorFunc not set")
			}
			// 真实 sarama 校验：修复前此处必然失败。
			if err := cfg.Validate(); err != nil {
				t.Fatalf("sarama config Validate() failed: %v", err)
			}
		})
	}
}

// TestXDGSCRAMClientHandshake 用 xdg-go/scram 的服务端实现跑完整
// SCRAM 挑战-应答，验证注入的客户端生成器产出可用的认证客户端。
func TestXDGSCRAMClientHandshake(t *testing.T) {
	for _, tc := range []struct {
		name string
		hash scram.HashGeneratorFcn
	}{
		{"SHA-256", scram.SHA256},
		{"SHA-512", scram.SHA512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSCRAMClientGenerator(tc.hash)()
			if err := client.Begin("user", "pass", ""); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			server, err := tc.hash.NewServer(scram.CredentialLookup(func(u string) (scram.StoredCredentials, error) {
				if u != "user" {
					return scram.StoredCredentials{}, fmt.Errorf("unknown user %q", u)
				}
				client, err := tc.hash.NewClient("user", "pass", "")
				if err != nil {
					return scram.StoredCredentials{}, err
				}
				return client.GetStoredCredentials(scram.KeyFactors{Salt: "salt", Iters: 4096}), nil
			}))
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			serverConv := server.NewConversation()

			clientFirst, err := client.Step("")
			if err != nil {
				t.Fatalf("client-first: %v", err)
			}
			serverFirst, err := serverConv.Step(clientFirst)
			if err != nil {
				t.Fatalf("server-first: %v", err)
			}
			clientFinal, err := client.Step(serverFirst)
			if err != nil {
				t.Fatalf("client-final: %v", err)
			}
			serverFinal, err := serverConv.Step(clientFinal)
			if err != nil {
				t.Fatalf("server-final: %v", err)
			}
			if _, err := client.Step(serverFinal); err != nil {
				t.Fatalf("client validation: %v", err)
			}
			if !client.Done() {
				t.Fatal("client conversation not done after successful handshake")
			}
		})
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
