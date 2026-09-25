package kafka

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/IBM/sarama"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// MetricsOptions 是 consumer lag 指标导出配置（kafka 段的保留键 `metrics`，
// 不是逻辑 topic 名）。
type MetricsOptions struct {
	// Enabled 为 false 时不启动采集；**默认 false（显式启用）**：采集会周期性
	// 查询 broker 元数据与消费组 offset，且仅在接入 meter provider（如
	// contrib/telemetry）后才有意义。
	Enabled *bool `mapstructure:"enabled"`
	// Interval 是采集间隔，默认 30s；过小会加重 broker 元数据 / offset 查询。
	Interval time.Duration `mapstructure:"interval"`
}

// metricsConfig 解析指标配置默认值。
func (o Options) metricsConfig() (enabled bool, interval time.Duration) {
	enabled, interval = false, 30*time.Second
	if o.Metrics != nil {
		if o.Metrics.Enabled != nil {
			enabled = *o.Metrics.Enabled
		}
		if o.Metrics.Interval > 0 {
			interval = o.Metrics.Interval
		}
	}
	return enabled, interval
}

const (
	// meterName 是 Kafka Transport 的 otel instrumentation scope 名。
	meterName = "github.com/lynx-go/lynx/contrib/watermill-kafka"
	// lagMetricName 是 consumer lag 指标名（高水位 - 已提交 offset，单位：条）。
	lagMetricName = "lynx.kafka.consumer.lag"
)

// offsetReader / groupOffsetReader 是 lag 采集所需的 sarama 能力（接口便于
// 单测注入假件；生产实现为 *sarama.Client / *sarama.ClusterAdmin）。
type offsetReader interface {
	Partitions(topic string) ([]int32, error)
	GetOffset(topic string, partition int32, which int64) (int64, error)
	Close() error
}

type groupOffsetReader interface {
	ListConsumerGroupOffsets(group string, topicPartitions map[string][]int32) (*sarama.OffsetFetchResponse, error)
	Close() error
}

// lagCollector 周期性采集「高水位 - 已提交 offset」，按（物理 topic × 分区 ×
// 消费组）记录为 lynx.kafka.consumer.lag。从未提交 offset（Offset < 0）的
// 分区本轮跳过——避免把「尚无提交」误报为满 backlog。
//
// 采集连接按（brokers × 认证指纹）分组共享，随 Transport 的停止 ctx 关闭；
// 采集失败只记 Warn 并继续其余 topic（监控面不得影响数据面）。
type lagCollector struct {
	opts     Options
	logger   *slog.Logger
	interval time.Duration

	// 工厂 seam（测试注入假件）。
	newClient func(brokers []string, sasl *SASLOptions, tlsOpts *TLSOptions) (offsetReader, error)
	newAdmin  func(brokers []string, sasl *SASLOptions, tlsOpts *TLSOptions) (groupOffsetReader, error)
}

func newLagCollector(opts Options, logger *slog.Logger) *lagCollector {
	return &lagCollector{
		opts:   opts,
		logger: logger,
		newClient: func(brokers []string, sasl *SASLOptions, tlsOpts *TLSOptions) (offsetReader, error) {
			cfg, err := lagSaramaConfig(sasl, tlsOpts)
			if err != nil {
				return nil, err
			}
			return sarama.NewClient(brokers, cfg)
		},
		newAdmin: func(brokers []string, sasl *SASLOptions, tlsOpts *TLSOptions) (groupOffsetReader, error) {
			cfg, err := lagSaramaConfig(sasl, tlsOpts)
			if err != nil {
				return nil, err
			}
			return sarama.NewClusterAdmin(brokers, cfg)
		},
	}
}

// lagSaramaConfig 是采集客户端的 sarama 配置（复用集群级认证）。
func lagSaramaConfig(sasl *SASLOptions, tlsOpts *TLSOptions) (*sarama.Config, error) {
	cfg := sarama.NewConfig()
	cfg.ClientID = "lynx-kafka-lag"
	if err := applyAuth(cfg, sasl, tlsOpts); err != nil {
		return nil, err
	}
	return cfg, nil
}

// run 阻塞采集直到 ctx 取消（Transport 停止 ctx）。
func (c *lagCollector) run(ctx context.Context) {
	gauge, err := otel.GetMeterProvider().Meter(meterName).Int64Gauge(lagMetricName,
		metric.WithDescription("Kafka consumer lag (high watermark - committed offset)"),
		metric.WithUnit("{message}"))
	if err != nil {
		c.logger.Warn("kafka: lag metric instrument creation failed, lag metrics disabled", "error", err)
		return
	}

	clients := map[string]offsetReader{}
	admins := map[string]groupOffsetReader{}
	defer func() {
		for _, cl := range clients {
			_ = cl.Close()
		}
		for _, ad := range admins {
			_ = ad.Close()
		}
	}()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.collect(ctx, gauge, clients, admins)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx, gauge, clients, admins)
		}
	}
}

func (c *lagCollector) collect(ctx context.Context, gauge metric.Int64Gauge, clients map[string]offsetReader, admins map[string]groupOffsetReader) {
	// 稳定遍历顺序（map 随机序对日志与测试不友好）。
	for _, name := range slices.Sorted(maps.Keys(c.opts.Topics)) {
		to := c.opts.Topics[name]
		if to.Consumer == nil || to.Consumer.GroupID == "" {
			continue // 只发布 / 未配置消费组：无 lag 可观测。
		}
		key := strings.Join(to.Brokers, ",") + "|" + configFingerprint(nil, to.SASL, to.TLS)
		client, ok := c.clientFor(key, to, clients)
		if !ok {
			continue
		}
		admin, ok := c.adminFor(key, to, admins)
		if !ok {
			continue
		}
		for _, physical := range to.Topics {
			partitions, err := client.Partitions(physical)
			if err != nil {
				c.logger.Warn("kafka: lag metric partitions lookup failed",
					"topic", physical, "group", to.Consumer.GroupID, "error", err)
				continue
			}
			resp, err := admin.ListConsumerGroupOffsets(to.Consumer.GroupID, map[string][]int32{physical: partitions})
			if err != nil {
				c.logger.Warn("kafka: lag metric committed offset lookup failed",
					"topic", physical, "group", to.Consumer.GroupID, "error", err)
				continue
			}
			for _, partition := range partitions {
				hw, err := client.GetOffset(physical, partition, sarama.OffsetNewest)
				if err != nil {
					c.logger.Warn("kafka: lag metric high watermark lookup failed",
						"topic", physical, "partition", partition, "error", err)
					continue
				}
				block := resp.GetBlock(physical, partition)
				if block == nil || block.Offset < 0 {
					continue // 从未提交：本轮跳过（不是满 backlog）。
				}
				gauge.Record(ctx, hw-block.Offset, metric.WithAttributes(
					semconv.MessagingDestinationName(physical),
					semconv.MessagingConsumerGroupName(to.Consumer.GroupID),
					attribute.Int("messaging.kafka.partition", int(partition)),
				))
			}
		}
	}
}

func (c *lagCollector) clientFor(key string, to TopicOptions, clients map[string]offsetReader) (offsetReader, bool) {
	if cl, ok := clients[key]; ok {
		return cl, true
	}
	cl, err := c.newClient(to.Brokers, to.SASL, to.TLS)
	if err != nil {
		c.logger.Warn("kafka: lag metric client creation failed",
			"brokers", strings.Join(to.Brokers, ","), "error", err)
		return nil, false
	}
	clients[key] = cl
	return cl, true
}

func (c *lagCollector) adminFor(key string, to TopicOptions, admins map[string]groupOffsetReader) (groupOffsetReader, bool) {
	if ad, ok := admins[key]; ok {
		return ad, true
	}
	ad, err := c.newAdmin(to.Brokers, to.SASL, to.TLS)
	if err != nil {
		c.logger.Warn("kafka: lag metric admin creation failed",
			"brokers", strings.Join(to.Brokers, ","), "error", err)
		return nil, false
	}
	admins[key] = ad
	return ad, true
}
