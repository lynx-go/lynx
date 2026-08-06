// Package kafka 提供 Kafka Transport 组件：按逻辑 topic 配置集群、
// 物理主题与消费/发布参数，接入 pubsub.Broker。
//
// 配置驱动：NewFromConfig 从配置 "kafka" 段加载 Options 并创建 Transport；
// 段缺失或为空时返回 (nil, nil) 表示 Kafka 未启用——**返回 nil 时不得
// Register**（框架对 plain nil 组件注册会返回明确错误）。
package kafka

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
)

// Options 是 Kafka Transport 的配置；可用 app.Config().UnmarshalKey("kafka", &opts)
// 从配置文件整表加载，结构为 map[逻辑topic]TopicOptions。
type Options struct {
	Topics map[string]TopicOptions `mapstructure:",remain"`
}

// TopicOptions 是一个逻辑 topic 的完整配置。
type TopicOptions struct {
	Brokers  []string         `mapstructure:"brokers"`  // Kafka 集群地址，必填
	Topics   []string         `mapstructure:"topics"`   // 订阅的物理 topic 列表
	Consumer *ConsumerOptions `mapstructure:"consumer"` // nil = 该 topic 只发布
	Producer *ProducerOptions `mapstructure:"producer"` // nil = 该 topic 只订阅
	// SASL 认证配置（集群级）。同集群多 topic 的认证配置需一致：
	// 客户端按 brokers 分组共享，先构建者生效。
	SASL *SASLOptions `mapstructure:"sasl"`
	// TLS 配置（集群级），约束同 SASL。
	TLS *TLSOptions `mapstructure:"tls"`
}

// SASLOptions 配置 Kafka 客户端 SASL 认证。
type SASLOptions struct {
	Enabled  bool   `mapstructure:"enabled"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	// Mechanism 是 SASL 机制：PLAIN（缺省）/ SCRAM-SHA-256 / SCRAM-SHA-512。
	Mechanism string `mapstructure:"mechanism"`
}

// TLSOptions 配置 Kafka 客户端 TLS。
type TLSOptions struct {
	Enabled           bool   `mapstructure:"enabled"`
	InsecureSkipVerify bool  `mapstructure:"insecure_skip_verify"`
	// CAFile 是自签 CA 证书路径；为空时使用系统信任库。
	CAFile string `mapstructure:"ca_file"`
	// ServerName 覆盖 TLS 校验的主机名；为空时使用 broker 地址。
	ServerName string `mapstructure:"server_name"`
}

// ConsumerOptions 是消费侧配置。
type ConsumerOptions struct {
	GroupID   string `mapstructure:"group_id"`
	Instances int    `mapstructure:"instances"`
	// CommitInterval 是 offset 自动提交间隔 → sarama Consumer.Offsets.AutoCommit.Interval。
	// AutoCommit.Enable 为 false 时无效（watermill 每条消息 Ack 即显式提交）。
	CommitInterval time.Duration `mapstructure:"commit_interval"`
	// AutoCommitEnabled 是否自动提交 offset，nil = 保持 sarama 默认 true；
	// false = watermill 在每条消息 Ack 时显式提交（CommitInterval 不生效）。
	AutoCommitEnabled *bool `mapstructure:"auto_commit_enabled"`
	// InitialOffset 是首次消费的初始 offset：oldest 或 newest（缺省 newest）
	// → sarama Consumer.Offsets.Initial（OffsetOldest / OffsetNewest）。
	InitialOffset string `mapstructure:"initial_offset"`
	LogMessage    bool   `mapstructure:"log_message"`
	// NackResendSleep 是 Nack 后消息重投的等待时长 → watermill SubscriberConfig.NackResendSleep。
	NackResendSleep time.Duration `mapstructure:"nack_resend_sleep"`
	// ReconnectRetrySleep 是重连失败后的下次重试间隔 → watermill SubscriberConfig.ReconnectRetrySleep。
	ReconnectRetrySleep time.Duration `mapstructure:"reconnect_retry_sleep"`
	// SessionTimeout 是消费组会话超时 → sarama Consumer.Group.Session.Timeout。
	SessionTimeout time.Duration `mapstructure:"session_timeout"`
	// HeartbeatInterval 是消费组心跳间隔 → sarama Consumer.Group.Heartbeat.Interval。
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
	// FetchMinBytes 是单次 fetch 的最小字节数 → sarama Consumer.Fetch.Min。
	FetchMinBytes int32 `mapstructure:"fetch_min_bytes"`
	// FetchMaxBytes 是单次 fetch 的最大字节数 → sarama Consumer.Fetch.Max。
	FetchMaxBytes int32 `mapstructure:"fetch_max_bytes"`
	// FetchMaxWait 是 broker 凑满 FetchMinBytes 的最长等待 → sarama Consumer.MaxWaitTime。
	FetchMaxWait time.Duration `mapstructure:"fetch_max_wait"`
	// ClientID 是客户端标识 → sarama ClientID。
	ClientID string `mapstructure:"client_id"`
}

// ProducerOptions 是发布侧配置。
type ProducerOptions struct {
	Topic      string `mapstructure:"topic"` // 发布物理 topic，缺省 = Topics[0]
	LogMessage bool   `mapstructure:"log_message"`
	// BatchSize 是批量攒够多少条消息后发送 → sarama Producer.Flush.Messages。
	BatchSize int `mapstructure:"batch_size"`
	// RequiredAcks 是 broker 应答级别（0=NoResponse/1=WaitForLocal/-1=WaitForAll，
	// 0 视为未设置）→ sarama Producer.RequiredAcks。
	RequiredAcks int16 `mapstructure:"required_acks"`
	// RetryMax 是发送失败的最大重试次数 → sarama Producer.Retry.Max。
	RetryMax int `mapstructure:"retry_max"`
	// Timeout 是 broker 等待 RequiredAcks 的最长时长 → sarama Producer.Timeout。
	Timeout time.Duration `mapstructure:"timeout"`
	// FlushBytes 是批量攒够多少字节后发送 → sarama Producer.Flush.Bytes。
	FlushBytes int `mapstructure:"flush_bytes"`
	// FlushFrequency 是批量消息的最长滞留时长 → sarama Producer.Flush.Frequency。
	FlushFrequency time.Duration `mapstructure:"flush_frequency"`
	// Compression 是压缩算法（none/gzip/snappy/lz4/zstd）→ sarama Producer.Compression。
	Compression string `mapstructure:"compression"`
	// ClientID 是客户端标识 → sarama ClientID。
	ClientID string `mapstructure:"client_id"`
}

// Transport 是 Kafka 后端组件：内部按 brokers 分组客户端，
// 订阅按（消费组 × 物理 topic × 实例数）展开后 fan-in。
type Transport struct {
	opts Options
	// logger 是组件日志实例：Init(ctx) 时从 ctx.Logger 取，未 Init
	//（脱离框架单用）时回落 slog.Default()。
	logger *slog.Logger

	mu          sync.Mutex
	publishers  map[string]message.Publisher  // key: brokers 列表
	subscribers map[string]message.Subscriber // key: "brokers|group"
	// pubSaramaConfigs/subSaramaConfigs 按侧缓存 sarama.Config：consumer 与
	// producer 参数正交（Config 的 Consumer/Producer 段），各侧独立构建，
	// 避免同集群两侧参数互斥、静默丢配置。
	// 缓存 key 仅按 brokers 分组：同 brokers 的多 topic 若配置了不同的
	// producer/consumer 参数，先构建者生效，后配置被静默忽略（P2-3 已知语义，
	// 与 SASL/TLS 的集群级约束一致）。
	pubSaramaConfigs map[string]*sarama.Config // key: brokers 列表
	subSaramaConfigs map[string]*sarama.Config // key: brokers 列表

	// 客户端工厂 seam：测试注入 fake。
	newPublisher  func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error)
	newSubscriber func(p subscriberParams, logger watermill.LoggerAdapter) (message.Subscriber, error)

	running atomic.Bool
	// stopped 标记 Stop 已执行：Stop 后 Publish 返回框架级错误，
	// 而非命中缓存的已关闭客户端报 sarama "client is closed"（P2-4）。
	stopped atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// subscriberParams 是 newSubscriber seam 的参数：sarama.Config 之外的
// watermill 订阅参数也在此传递。
type subscriberParams struct {
	brokers             []string
	group               string
	sarama              *sarama.Config
	nackResendSleep     time.Duration
	reconnectRetrySleep time.Duration
}

// NewTransport 创建 Kafka Transport。
func NewTransport(opts Options) (*Transport, error) {
	t := &Transport{
		opts:             opts,
		logger:           slog.Default(),
		publishers:       map[string]message.Publisher{},
		subscribers:      map[string]message.Subscriber{},
		pubSaramaConfigs: map[string]*sarama.Config{},
		subSaramaConfigs: map[string]*sarama.Config{},
		newPublisher: func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error) {
			return watermillkafka.NewPublisher(watermillkafka.PublisherConfig{
				Brokers:               brokers,
				OverwriteSaramaConfig: cfg,
				// Marshaler 必填：缺省时 NewPublisher 直接报 "missing marshaler"。
				Marshaler: watermillkafka.DefaultMarshaler{},
			}, logger)
		},
		newSubscriber: func(p subscriberParams, logger watermill.LoggerAdapter) (message.Subscriber, error) {
			subCfg := watermillkafka.SubscriberConfig{
				Brokers:               p.brokers,
				ConsumerGroup:         p.group,
				OverwriteSaramaConfig: p.sarama,
				NackResendSleep:       p.nackResendSleep,
				ReconnectRetrySleep:   p.reconnectRetrySleep,
			}
			return watermillkafka.NewSubscriber(subCfg, logger)
		},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t, nil
}

// Name 返回组件名称 "kafka-transport"。
func (t *Transport) Name() string { return "kafka-transport" }

// Init 校验配置并记录日志实例。
func (t *Transport) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		t.logger = ctx.Logger("component", t.Name())
	}
	for name, topic := range t.opts.Topics {
		if len(topic.Brokers) == 0 {
			return fmt.Errorf("kafka: topic %q has no brokers", name)
		}
		if len(topic.Topics) == 0 {
			return fmt.Errorf("kafka: topic %q has no physical topics", name)
		}
	}
	return nil
}

// Start 标记运行并阻塞至组件停止。
func (t *Transport) Start(ctx context.Context) error {
	t.running.Store(true)
	<-t.ctx.Done()
	t.running.Store(false)
	return nil
}

// Stop 关闭全部客户端并取消组件上下文；关闭错误聚合返回。
func (t *Transport) Stop(ctx context.Context) error {
	t.stopped.Store(true)
	t.mu.Lock()
	defer t.mu.Unlock()
	var closeErrors lynx.ShutdownErrors
	for _, p := range t.publishers {
		if err := p.Close(); err != nil {
			t.logger.ErrorContext(ctx, "error closing kafka publisher", "error", err)
			closeErrors.Add(err)
		}
	}
	for _, s := range t.subscribers {
		if err := s.Close(); err != nil {
			t.logger.ErrorContext(ctx, "error closing kafka subscriber", "error", err)
			closeErrors.Add(err)
		}
	}
	t.running.Store(false)
	t.cancel()
	if closeErrors.HasErrors() {
		return &closeErrors
	}
	return nil
}

// CheckHealth 报告 Transport 是否在运行。
func (t *Transport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("kafka transport is not running")
	}
	return nil
}

// Topics 返回配置的逻辑 topic 全集（Broker 自动路由用）。
func (t *Transport) Topics() []string {
	names := make([]string, 0, len(t.opts.Topics))
	for name := range t.opts.Topics {
		names = append(names, name)
	}
	return names
}

func (t *Transport) watermillLogger() watermill.LoggerAdapter {
	if t.logger == nil {
		return watermill.NopLogger{}
	}
	return watermill.NewSlogLogger(t.logger)
}

// Publish 将消息发布到逻辑 topic 对应的物理 topic；ctx 用于传播 trace/元数据。
func (t *Transport) Publish(ctx context.Context, topic string, msgs ...*message.Message) error {
	if t.stopped.Load() {
		return errors.New("kafka transport is stopped")
	}
	to, ok := t.opts.Topics[topic]
	if !ok {
		return fmt.Errorf("kafka: topic %q not configured", topic)
	}
	if to.Producer == nil {
		return fmt.Errorf("kafka: topic %q has no producer config", topic)
	}
	physical := to.Producer.Topic
	if physical == "" {
		if len(to.Topics) == 0 {
			return fmt.Errorf("kafka: topic %q has no physical topics", topic)
		}
		physical = to.Topics[0]
	}
	p, err := t.publisherFor(to.Brokers, to.Producer, to.SASL, to.TLS)
	if err != nil {
		return err
	}
	if to.Producer.LogMessage {
		for _, msg := range msgs {
			t.logger.DebugContext(ctx, "sending kafka message", "message", string(msg.Payload), "topic", physical)
		}
	}
	return p.Publish(physical, msgs...)
}

// Subscribe 订阅逻辑 topic：按（消费组 × 物理 topic × 实例数）展开，
// 全部消息 fan-in 到单一返回 channel。
func (t *Transport) Subscribe(ctx context.Context, topic string, opts pubsub.SubscriptionOptions) (<-chan *message.Message, error) {
	if t.stopped.Load() {
		return nil, errors.New("kafka transport is stopped")
	}
	to, ok := t.opts.Topics[topic]
	if !ok {
		return nil, fmt.Errorf("kafka: topic %q not configured", topic)
	}
	if to.Consumer == nil {
		return nil, fmt.Errorf("kafka: topic %q has no consumer config", topic)
	}
	group := opts.Group
	if group == "" {
		group = to.Consumer.GroupID
	}
	if group == "" {
		return nil, fmt.Errorf("kafka: consumer group required for topic %q (config consumer.group_id or WithGroup)", topic)
	}
	instances := opts.Instances
	if instances < 1 {
		instances = to.Consumer.Instances
	}
	if instances < 1 {
		instances = 1
	}

	sub, err := t.subscriberFor(to.Brokers, group, to.Consumer, to.SASL, to.TLS)
	if err != nil {
		return nil, err
	}

	// 派生 ctx：展开中途任一 Subscribe 失败时取消它，已建立的子订阅
	// channel 随之关闭，避免泄漏。
	subCtx, cancel := context.WithCancel(ctx)
	chans := make([]<-chan *message.Message, 0, len(to.Topics)*instances)
	for _, physical := range to.Topics {
		for i := 0; i < instances; i++ {
			ch, err := sub.Subscribe(subCtx, physical)
			if err != nil {
				cancel()
				return nil, err
			}
			if to.Consumer.LogMessage {
				ch = t.logMessages(subCtx, physical, ch)
			}
			chans = append(chans, ch)
		}
	}
	return fanIn(chans, cancel), nil
}

// compressionCodecs 是配置字符串到 sarama 压缩算法的映射。
var compressionCodecs = map[string]sarama.CompressionCodec{
	"none":   sarama.CompressionNone,
	"gzip":   sarama.CompressionGZIP,
	"snappy": sarama.CompressionSnappy,
	"lz4":    sarama.CompressionLZ4,
	"zstd":   sarama.CompressionZSTD,
}

// applyAuth 将集群级认证（SASL/TLS）应用到 sarama.Config。
func applyAuth(cfg *sarama.Config, sasl *SASLOptions, tlsOpts *TLSOptions) error {
	if sasl != nil && sasl.Enabled {
		cfg.Net.SASL.Enable = true
		cfg.Net.SASL.User = sasl.User
		cfg.Net.SASL.Password = sasl.Password
		switch strings.ToUpper(sasl.Mechanism) {
		case "", "PLAIN":
			cfg.Net.SASL.Mechanism = sarama.SASLTypePlaintext
		case "SCRAM-SHA-256":
			cfg.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256
			cfg.Net.SASL.SCRAMClientGeneratorFunc = newSCRAMClientGenerator(sha256.New)
		case "SCRAM-SHA-512":
			cfg.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
			cfg.Net.SASL.SCRAMClientGeneratorFunc = newSCRAMClientGenerator(sha512.New)
		default:
			return fmt.Errorf("kafka: unsupported sasl mechanism %q (PLAIN/SCRAM-SHA-256/SCRAM-SHA-512)", sasl.Mechanism)
		}
	}
	if tlsOpts != nil && tlsOpts.Enabled {
		tc := &tls.Config{InsecureSkipVerify: tlsOpts.InsecureSkipVerify, ServerName: tlsOpts.ServerName}
		if tlsOpts.CAFile != "" {
			caCert, err := os.ReadFile(tlsOpts.CAFile)
			if err != nil {
				return fmt.Errorf("kafka: read ca_file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caCert) {
				return fmt.Errorf("kafka: ca_file %q contains no valid certificates", tlsOpts.CAFile)
			}
			tc.RootCAs = pool
		}
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tc
	}
	return nil
}

// buildPublisherConfig 构建并缓存发布侧 sarama.Config（按 brokers 分组）。
// consumer 与 producer 参数正交，两侧独立构建，互不覆盖。
// 调用方必须已持有 t.mu。
func (t *Transport) buildPublisherConfig(brokers []string, producer *ProducerOptions, sasl *SASLOptions, tlsOpts *TLSOptions) (*sarama.Config, error) {
	key := strings.Join(brokers, ",")
	if cfg, ok := t.pubSaramaConfigs[key]; ok {
		return cfg, nil
	}
	cfg := sarama.NewConfig()
	// SyncProducer 必需项：watermill-kafka 仅在 OverwriteSaramaConfig 为 nil
	// 时才应用 DefaultSaramaSyncPublisherConfig（唯一设置 Successes=true 的
	// 位置），此处显式设置，否则 NewSyncProducer 配置校验直接失败。
	cfg.Producer.Return.Successes = true
	cfg.Producer.Return.Errors = true
	if producer != nil {
		if producer.BatchSize > 0 {
			cfg.Producer.Flush.Messages = producer.BatchSize
		}
		if producer.RequiredAcks != 0 {
			cfg.Producer.RequiredAcks = sarama.RequiredAcks(producer.RequiredAcks)
		}
		if producer.RetryMax > 0 {
			cfg.Producer.Retry.Max = producer.RetryMax
		}
		if producer.Timeout > 0 {
			cfg.Producer.Timeout = producer.Timeout
		}
		if producer.FlushBytes > 0 {
			cfg.Producer.Flush.Bytes = producer.FlushBytes
		}
		if producer.FlushFrequency > 0 {
			cfg.Producer.Flush.Frequency = producer.FlushFrequency
		}
		if producer.Compression != "" {
			codec, ok := compressionCodecs[producer.Compression]
			if !ok {
				return nil, fmt.Errorf("kafka: unsupported compression %q (none/gzip/snappy/lz4/zstd)", producer.Compression)
			}
			cfg.Producer.Compression = codec
		}
		if producer.ClientID != "" {
			cfg.ClientID = producer.ClientID
		}
	}
	if err := applyAuth(cfg, sasl, tlsOpts); err != nil {
		return nil, err
	}
	t.pubSaramaConfigs[key] = cfg
	return cfg, nil
}

// buildSubscriberConfig 构建并缓存订阅侧 sarama.Config（按 brokers 分组）。
// 调用方必须已持有 t.mu。
func (t *Transport) buildSubscriberConfig(brokers []string, consumer *ConsumerOptions, sasl *SASLOptions, tlsOpts *TLSOptions) (*sarama.Config, error) {
	key := strings.Join(brokers, ",")
	if cfg, ok := t.subSaramaConfigs[key]; ok {
		return cfg, nil
	}
	cfg := sarama.NewConfig()
	if consumer != nil {
		if consumer.AutoCommitEnabled != nil {
			cfg.Consumer.Offsets.AutoCommit.Enable = *consumer.AutoCommitEnabled
		}
		if consumer.CommitInterval > 0 {
			cfg.Consumer.Offsets.AutoCommit.Interval = consumer.CommitInterval
		}
		if consumer.InitialOffset != "" {
			switch consumer.InitialOffset {
			case "oldest":
				cfg.Consumer.Offsets.Initial = sarama.OffsetOldest
			case "newest":
				cfg.Consumer.Offsets.Initial = sarama.OffsetNewest
			default:
				return nil, fmt.Errorf("kafka: invalid initial_offset %q (want oldest or newest)", consumer.InitialOffset)
			}
		}
		if consumer.SessionTimeout > 0 {
			cfg.Consumer.Group.Session.Timeout = consumer.SessionTimeout
		}
		if consumer.HeartbeatInterval > 0 {
			cfg.Consumer.Group.Heartbeat.Interval = consumer.HeartbeatInterval
		}
		if consumer.FetchMinBytes > 0 {
			cfg.Consumer.Fetch.Min = consumer.FetchMinBytes
		}
		if consumer.FetchMaxBytes > 0 {
			cfg.Consumer.Fetch.Max = consumer.FetchMaxBytes
		}
		if consumer.FetchMaxWait > 0 {
			cfg.Consumer.MaxWaitTime = consumer.FetchMaxWait
		}
		if consumer.ClientID != "" {
			cfg.ClientID = consumer.ClientID
		}
	}
	if err := applyAuth(cfg, sasl, tlsOpts); err != nil {
		return nil, err
	}
	t.subSaramaConfigs[key] = cfg
	return cfg, nil
}

func (t *Transport) publisherFor(brokers []string, producer *ProducerOptions, sasl *SASLOptions, tlsOpts *TLSOptions) (message.Publisher, error) {
	key := strings.Join(brokers, ",")
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.publishers[key]; ok {
		return p, nil
	}
	cfg, err := t.buildPublisherConfig(brokers, producer, sasl, tlsOpts)
	if err != nil {
		return nil, err
	}
	p, err := t.newPublisher(brokers, cfg, t.watermillLogger())
	if err != nil {
		return nil, err
	}
	t.publishers[key] = p
	return p, nil
}

func (t *Transport) subscriberFor(brokers []string, group string, consumer *ConsumerOptions, sasl *SASLOptions, tlsOpts *TLSOptions) (message.Subscriber, error) {
	key := strings.Join(brokers, ",") + "|" + group
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.subscribers[key]; ok {
		return s, nil
	}
	cfg, err := t.buildSubscriberConfig(brokers, consumer, sasl, tlsOpts)
	if err != nil {
		return nil, err
	}
	params := subscriberParams{brokers: brokers, group: group, sarama: cfg}
	if consumer != nil {
		params.nackResendSleep = consumer.NackResendSleep
		params.reconnectRetrySleep = consumer.ReconnectRetrySleep
	}
	s, err := t.newSubscriber(params, t.watermillLogger())
	if err != nil {
		return nil, err
	}
	t.subscribers[key] = s
	return s, nil
}

// logMessages 包装订阅 channel：每条消息按 ConsumerOptions.LogMessage
// 输出 debug 日志（对齐 Producer 侧 LogMessage 语义）。
func (t *Transport) logMessages(ctx context.Context, physical string, in <-chan *message.Message) <-chan *message.Message {
	out := make(chan *message.Message)
	go func() {
		defer close(out)
		for msg := range in {
			t.logger.DebugContext(ctx, "received kafka message", "message", string(msg.Payload), "topic", physical)
			out <- msg
		}
	}()
	return out
}

// fanIn 合并多个订阅 channel 为单一 channel；全部输入关闭后关闭输出，
// 并调用 done（用于取消 Subscribe 的派生 ctx，释放子订阅）。
func fanIn(chans []<-chan *message.Message, done func()) <-chan *message.Message {
	out := make(chan *message.Message)
	var wg sync.WaitGroup
	for _, ch := range chans {
		wg.Add(1)
		go func(ch <-chan *message.Message) {
			defer wg.Done()
			for msg := range ch {
				out <- msg
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
		if done != nil {
			done()
		}
	}()
	return out
}

var _ pubsub.Transport = (*Transport)(nil)
var _ lynx.Component = (*Transport)(nil)
