// Package kafka 提供 Kafka Transport 组件：按逻辑 topic 配置集群、
// 物理主题与消费/发布参数，接入 pubsub.Broker。
package kafka

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/lynx-go/x/log"
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
	app  lynx.App

	mu            sync.Mutex
	publishers    map[string]message.Publisher  // key: brokers 列表
	subscribers   map[string]message.Subscriber // key: "brokers|group"
	saramaConfigs map[string]*sarama.Config     // key: brokers 列表（同集群共享客户端配置）

	// 客户端工厂 seam：测试注入 fake。
	newPublisher  func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error)
	newSubscriber func(p subscriberParams, logger watermill.LoggerAdapter) (message.Subscriber, error)

	running atomic.Bool
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
		opts:          opts,
		publishers:    map[string]message.Publisher{},
		subscribers:   map[string]message.Subscriber{},
		saramaConfigs: map[string]*sarama.Config{},
		newPublisher: func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error) {
			return watermillkafka.NewPublisher(watermillkafka.PublisherConfig{Brokers: brokers, OverwriteSaramaConfig: cfg}, logger)
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

// Init 校验配置并保存应用实例。
func (t *Transport) Init(app lynx.App) error {
	t.app = app
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

// Stop 关闭全部客户端并取消组件上下文。
func (t *Transport) Stop(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.publishers {
		if err := p.Close(); err != nil {
			log.ErrorContext(ctx, "error closing kafka publisher", err)
		}
	}
	for _, s := range t.subscribers {
		if err := s.Close(); err != nil {
			log.ErrorContext(ctx, "error closing kafka subscriber", err)
		}
	}
	t.running.Store(false)
	t.cancel()
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

func (t *Transport) logger() watermill.LoggerAdapter {
	if t.app == nil {
		return watermill.NopLogger{}
	}
	return watermill.NewSlogLogger(t.app.Logger("component", "kafka"))
}

// Publish 将消息发布到逻辑 topic 对应的物理 topic。
func (t *Transport) Publish(topic string, msgs ...*message.Message) error {
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
	p, err := t.publisherFor(to.Brokers, to.Producer)
	if err != nil {
		return err
	}
	if to.Producer.LogMessage && t.app != nil {
		for _, msg := range msgs {
			log.DebugContext(t.app.Context(), "sending kafka message", "message", string(msg.Payload), "topic", physical)
		}
	}
	return p.Publish(physical, msgs...)
}

// Subscribe 订阅逻辑 topic：按（消费组 × 物理 topic × 实例数）展开，
// 全部消息 fan-in 到单一返回 channel。
func (t *Transport) Subscribe(ctx context.Context, topic string, opts pubsub.SubscriptionOptions) (<-chan *message.Message, error) {
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

	sub, err := t.subscriberFor(to.Brokers, group, to.Consumer)
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

// buildSaramaConfig 按集群构建 sarama.Config：首个调用方（publisher 或
// subscriber）的便捷参数生效，同集群共享客户端配置。consumer/producer 为
// 对应侧的配置（可为 nil，nil 侧参数不映射）。非法 compression 返回 error。
// 调用方必须已持有 t.mu。
func (t *Transport) buildSaramaConfig(brokers []string, consumer *ConsumerOptions, producer *ProducerOptions) (*sarama.Config, error) {
	key := strings.Join(brokers, ",")
	if cfg, ok := t.saramaConfigs[key]; ok {
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
	t.saramaConfigs[key] = cfg
	return cfg, nil
}

func (t *Transport) publisherFor(brokers []string, producer *ProducerOptions) (message.Publisher, error) {
	key := strings.Join(brokers, ",")
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.publishers[key]; ok {
		return p, nil
	}
	cfg, err := t.buildSaramaConfig(brokers, nil, producer)
	if err != nil {
		return nil, err
	}
	p, err := t.newPublisher(brokers, cfg, t.logger())
	if err != nil {
		return nil, err
	}
	t.publishers[key] = p
	return p, nil
}

func (t *Transport) subscriberFor(brokers []string, group string, consumer *ConsumerOptions) (message.Subscriber, error) {
	key := strings.Join(brokers, ",") + "|" + group
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.subscribers[key]; ok {
		return s, nil
	}
	cfg, err := t.buildSaramaConfig(brokers, consumer, nil)
	if err != nil {
		return nil, err
	}
	params := subscriberParams{brokers: brokers, group: group, sarama: cfg}
	if consumer != nil {
		params.nackResendSleep = consumer.NackResendSleep
		params.reconnectRetrySleep = consumer.ReconnectRetrySleep
	}
	s, err := t.newSubscriber(params, t.logger())
	if err != nil {
		return nil, err
	}
	t.subscribers[key] = s
	return s, nil
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
