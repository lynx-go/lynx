package watermill

// DefaultMaxRedeliveries 是 MaxRedeliveries 未设置（0）时的默认重投上限。
const DefaultMaxRedeliveries = 10

// Options 是 watermill Bus 的扩展配置：eventbus.Options 自 v1.0 起冻结，
// 重投上限等 watermill 特有项集中在此，经 New 的可变参数注入，
// NewFromConfig 则从配置文件 "bus" 段装配。
type Options struct {
	// MaxRedeliveries 限制同一条消息（按 handler × message ID 计）在
	// handler 终态失败（重试耗尽）后的累计重投轮数：超过即记 Error 并
	// Ack 丢弃。上游 Transport 可能无限重投失败消息（Kafka ResendLoop
	// 默认 100ms 一轮），单分区顺序消费下毒消息会饿死整条队列，必须有
	// Bus 级止损。0 = DefaultMaxRedeliveries；负数 = 不设限（等价旧行为，
	// 不推荐）。
	MaxRedeliveries int
	// Topics 按主题覆盖 MaxRedeliveries（0 = 沿用 Bus 级配置）。
	Topics map[string]TopicConfig
}

// TopicConfig 是单主题的 watermill 扩展配置。
type TopicConfig struct {
	// MaxRedeliveries 覆盖该主题的重投上限；0 = 沿用 Bus 级，负数 = 不设限。
	MaxRedeliveries int
}

// Option 配置 watermill Bus 的扩展行为。
type Option func(*Options)

// WithMaxRedeliveries 设置 Bus 级重投上限（语义见 Options.MaxRedeliveries）。
func WithMaxRedeliveries(n int) Option {
	return func(o *Options) { o.MaxRedeliveries = n }
}

// WithTopicMaxRedeliveries 覆盖单主题的重投上限（优先于 Bus 级）。
func WithTopicMaxRedeliveries(topic string, n int) Option {
	return func(o *Options) {
		if o.Topics == nil {
			o.Topics = map[string]TopicConfig{}
		}
		o.Topics[topic] = TopicConfig{MaxRedeliveries: n}
	}
}

// maxRedeliveriesFor 解析 topic 的最终重投上限；ok=false 表示不设限
// （显式配置了负数）。主题级非零配置优先于 Bus 级。
func (b *Bus) maxRedeliveriesFor(topic string) (limit int, ok bool) {
	if tc, found := b.ext.Topics[topic]; found && tc.MaxRedeliveries != 0 {
		if tc.MaxRedeliveries < 0 {
			return 0, false
		}
		return tc.MaxRedeliveries, true
	}
	switch {
	case b.ext.MaxRedeliveries < 0:
		return 0, false
	case b.ext.MaxRedeliveries == 0:
		return DefaultMaxRedeliveries, true
	default:
		return b.ext.MaxRedeliveries, true
	}
}
