package watermill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
)

// KafkaOptions 复用原 kafka 段结构，简化后仅保留必要字段，完整配置可后续扩展。
type KafkaOptions struct {
	Topics map[string]KafkaTopicOptions `mapstructure:",remain"`
}

type KafkaTopicOptions struct {
	Brokers  []string               `mapstructure:"brokers"`
	Topics   []string               `mapstructure:"topics"`
	Consumer *KafkaConsumerOptions `mapstructure:"consumer"`
	Producer *KafkaProducerOptions `mapstructure:"producer"`
}

type KafkaConsumerOptions struct {
	GroupID   string `mapstructure:"group_id"`
	Instances int    `mapstructure:"instances"`
}

type KafkaProducerOptions struct {
	Topic string `mapstructure:"topic"`
}

// KafkaTransport 是基于 watermill-kafka 的 eventbus.Transport 实现。
type KafkaTransport struct {
	opts   KafkaOptions
	logger *slog.Logger

	mu          sync.Mutex
	publishers  map[string]message.Publisher
	subscribers map[string]message.Subscriber
	running     atomic.Bool
	stopped     atomic.Bool
	ctx         context.Context
	cancel      context.CancelFunc
}

func NewKafkaTransport(opts KafkaOptions) (*KafkaTransport, error) {
	t := &KafkaTransport{
		opts:        opts,
		logger:      slog.Default(),
		publishers:  map[string]message.Publisher{},
		subscribers: map[string]message.Subscriber{},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t, nil
}

func (t *KafkaTransport) Name() string { return "watermill-kafka" }

func (t *KafkaTransport) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		t.logger = ctx.Logger("service", t.Name())
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

func (t *KafkaTransport) Start(ctx context.Context) error {
	t.running.Store(true)
	select {
	case <-ctx.Done():
	case <-t.ctx.Done():
	}
	t.running.Store(false)
	return nil
}

func (t *KafkaTransport) Stop(ctx context.Context) error {
	t.stopped.Store(true)
	t.mu.Lock()
	defer t.mu.Unlock()
	var errs lynx.ShutdownErrors
	for _, p := range t.publishers {
		if err := p.Close(); err != nil {
			errs.Add(err)
		}
	}
	for _, s := range t.subscribers {
		if err := s.Close(); err != nil {
			errs.Add(err)
		}
	}
	t.running.Store(false)
	t.cancel()
	if errs.HasErrors() {
		return &errs
	}
	return nil
}

func (t *KafkaTransport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("kafka transport is not running")
	}
	return nil
}

func (t *KafkaTransport) Close() error {
	return t.Stop(context.Background())
}

func (t *KafkaTransport) Topics() []string {
	names := make([]string, 0, len(t.opts.Topics))
	for name := range t.opts.Topics {
		names = append(names, name)
	}
	return names
}

func (t *KafkaTransport) Publish(ctx context.Context, topic string, e *eventbus.RawEvent) error {
	if t.stopped.Load() {
		return errors.New("kafka transport is stopped")
	}
	opt, ok := t.opts.Topics[topic]
	if !ok {
		return fmt.Errorf("kafka: topic %q not configured", topic)
	}
	if opt.Producer == nil {
		return fmt.Errorf("kafka: topic %q has no producer", topic)
	}
	physical := opt.Producer.Topic
	if physical == "" && len(opt.Topics) > 0 {
		physical = opt.Topics[0]
	}
	p, err := t.publisherFor(opt.Brokers)
	if err != nil {
		return err
	}
	msg := toWatermill(e)
	// watermill-kafka publish expects topic string
	return p.Publish(physical, msg)
}

func (t *KafkaTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan *eventbus.RawEvent, error) {
	if t.stopped.Load() {
		return nil, errors.New("kafka transport is stopped")
	}
	opt, ok := t.opts.Topics[topic]
	if !ok {
		return nil, fmt.Errorf("kafka: topic %q not configured", topic)
	}
	if opt.Consumer == nil {
		return nil, fmt.Errorf("kafka: topic %q has no consumer", topic)
	}
	group := opts.Group
	if group == "" {
		group = opt.Consumer.GroupID
	}
	if group == "" {
		return nil, fmt.Errorf("kafka: consumer group required for topic %q", topic)
	}
	sub, err := t.subscriberFor(opt.Brokers, group)
	if err != nil {
		return nil, err
	}
	// fan-in for multiple physical topics
	out := make(chan *eventbus.RawEvent)
	go func() {
		defer close(out)
		// For simplicity, subscribe to first physical topic only in this minimal impl
		if len(opt.Topics) == 0 {
			return
		}
		ch, err := sub.Subscribe(ctx, opt.Topics[0])
		if err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				raw := fromWatermill(msg)
				raw.Topic = topic
				select {
				case out <- raw:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (t *KafkaTransport) publisherFor(brokers []string) (message.Publisher, error) {
	// Simplified: create new publisher per brokers, no config caching
	key := brokersKey(brokers)
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.publishers[key]; ok {
		return p, nil
	}
	cfg := sarama.NewConfig()
	cfg.Producer.Return.Successes = true
	cfg.Producer.Return.Errors = true
	p, err := watermillkafka.NewPublisher(watermillkafka.PublisherConfig{
		Brokers:               brokers,
		OverwriteSaramaConfig: cfg,
		Marshaler:             watermillkafka.DefaultMarshaler{},
	}, watermill.NewSlogLogger(t.logger))
	if err != nil {
		return nil, err
	}
	t.publishers[key] = p
	return p, nil
}

func (t *KafkaTransport) subscriberFor(brokers []string, group string) (message.Subscriber, error) {
	key := brokersKey(brokers) + "|" + group
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.subscribers[key]; ok {
		return s, nil
	}
	cfg := sarama.NewConfig()
	s, err := watermillkafka.NewSubscriber(watermillkafka.SubscriberConfig{
		Brokers:               brokers,
		ConsumerGroup:         group,
		OverwriteSaramaConfig: cfg,
		Unmarshaler:           watermillkafka.DefaultMarshaler{},
	}, watermill.NewSlogLogger(t.logger))
	if err != nil {
		return nil, err
	}
	t.subscribers[key] = s
	return s, nil
}

func brokersKey(brokers []string) string {
	// Simple join
	key := ""
	for i, b := range brokers {
		if i > 0 {
			key += ","
		}
		key += b
	}
	return key
}

var _ eventbus.Transport = (*KafkaTransport)(nil)
var _ lynx.Service = (*KafkaTransport)(nil)
