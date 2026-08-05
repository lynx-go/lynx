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
	GroupID        string        `mapstructure:"group_id"`
	Instances      int           `mapstructure:"instances"`
	CommitInterval time.Duration `mapstructure:"commit_interval"`
	LogMessage     bool          `mapstructure:"log_message"`
}

// ProducerOptions 是发布侧配置。
type ProducerOptions struct {
	Topic      string `mapstructure:"topic"` // 发布物理 topic，缺省 = Topics[0]
	LogMessage bool   `mapstructure:"log_message"`
	BatchSize  int    `mapstructure:"batch_size"`
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
	newSubscriber func(brokers []string, group string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Subscriber, error)

	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
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
		newSubscriber: func(brokers []string, group string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Subscriber, error) {
			subCfg := watermillkafka.SubscriberConfig{Brokers: brokers, ConsumerGroup: group, OverwriteSaramaConfig: cfg}
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
	batchSize := 0
	if to.Producer.BatchSize > 0 {
		batchSize = to.Producer.BatchSize
	}
	p, err := t.publisherFor(to.Brokers, batchSize)
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

	commitInterval := time.Duration(0)
	if to.Consumer.CommitInterval > 0 {
		commitInterval = to.Consumer.CommitInterval
	}
	sub, err := t.subscriberFor(to.Brokers, group, commitInterval)
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

// buildSaramaConfig 按集群构建 sarama.Config：首个 topic 的便捷参数
// （CommitInterval / BatchSize）生效，同集群共享客户端配置。
// 调用方必须已持有 t.mu。
func (t *Transport) buildSaramaConfig(brokers []string, commitInterval time.Duration, batchSize int) *sarama.Config {
	key := strings.Join(brokers, ",")
	if cfg, ok := t.saramaConfigs[key]; ok {
		return cfg
	}
	cfg := sarama.NewConfig()
	if commitInterval > 0 {
		cfg.Consumer.Offsets.CommitInterval = commitInterval
	}
	if batchSize > 0 {
		cfg.Producer.Flush.Messages = batchSize
	}
	t.saramaConfigs[key] = cfg
	return cfg
}

func (t *Transport) publisherFor(brokers []string, batchSize int) (message.Publisher, error) {
	key := strings.Join(brokers, ",")
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.publishers[key]; ok {
		return p, nil
	}
	p, err := t.newPublisher(brokers, t.buildSaramaConfig(brokers, 0, batchSize), t.logger())
	if err != nil {
		return nil, err
	}
	t.publishers[key] = p
	return p, nil
}

func (t *Transport) subscriberFor(brokers []string, group string, commitInterval time.Duration) (message.Subscriber, error) {
	key := strings.Join(brokers, ",") + "|" + group
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.subscribers[key]; ok {
		return s, nil
	}
	s, err := t.newSubscriber(brokers, group, t.buildSaramaConfig(brokers, commitInterval, 0), t.logger())
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
