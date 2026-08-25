package watermill

import (
	"sync"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/eventbus"
)

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

// redeliveryLimiter 按（handler × 消息 ID）计数终态失败轮数。有界：容量
// 满后按插入序环形淘汰最老条目——计数是止损机制而非精确语义，被淘汰的
// 陈旧键重新计数是可接受的误差，换取消耗与流量无关的常数内存（防止键
// 空间无限增长撑爆 map）。
type redeliveryLimiter struct {
	mu     sync.Mutex
	counts map[string]int
	order  []string // 环形槽位：新键顶掉最老槽位
	pos    int
}

func newRedeliveryLimiter(capacity int) *redeliveryLimiter {
	if capacity <= 0 {
		capacity = 4096
	}
	return &redeliveryLimiter{
		counts: make(map[string]int, capacity),
		order:  make([]string, capacity),
	}
}

// limiterKey 拼接计数键：handlerName + "|" + 消息 ID（复审-4）。同一消息
// 可能同时投给多个 handler（如 Kafka 上两个不同消费组各收一份），纯消息
// ID 的 Bus 级共享键会让成功侧的 success 清零失败侧的累计计数，毒消息
// 永不达上限。handlerName 在 Bus 内全局唯一（handlerNames 查重），足以
// 区分同消息的并发订阅。
func limiterKey(handlerName, id string) string {
	return handlerName + "|" + id
}

// failure 记录该 handler 对该消息的一轮终态失败，返回累计轮数（含本次）。
func (r *redeliveryLimiter) failure(handlerName, id string) int {
	if id == "" {
		// 无 ID 的消息无法跨轮追踪，返回一个大值让调用方立即止损。
		return 1 << 30
	}
	key := limiterKey(handlerName, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.counts[key]; !ok {
		if victim := r.order[r.pos]; victim != "" {
			delete(r.counts, victim)
		}
		r.order[r.pos] = key
		r.pos = (r.pos + 1) % len(r.order)
	}
	r.counts[key]++
	return r.counts[key]
}

// success 清除该 handler 对该消息的计数：消息处理成功即生命周期结束，
// 腾出容量给活跃键，也避免同 ID 的陈旧计数误伤后续投递（如上游按 ID
// 重发的新消息）。键含 handlerName，只清自身，不动其他 handler 对同一
// 消息的计数（见 limiterKey）。
func (r *redeliveryLimiter) success(handlerName, id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.counts, limiterKey(handlerName, id))
}

// redeliveryMiddleware 返回按投递轮次计数毒消息的中间件（WK-02）。
// 内层 handler（含 Retry）返回错误即是一轮终态失败：Router 会 Nack →
// Transport 重投（Kafka ResendLoop）→ 再进 handler……无 DLQ 时唯一止损
// 是超过上限后丢弃：记 Error 留痕并返回 nil（Router 视为成功并 Ack），
// 阻断重投循环。必须先于 Retry 中间件添加（先添加者位于调用栈最外层），
// 这样 Retry 的内层多次重试不会被重复计数，每轮只计一次；计数按
// handlerName 隔离（复审-4，见 limiterKey）。
func (b *Bus) redeliveryMiddleware(handlerName, topic string, limit int) message.HandlerMiddleware {
	return func(h message.HandlerFunc) message.HandlerFunc {
		return func(msg *message.Message) ([]*message.Message, error) {
			produced, err := h(msg)
			if err == nil {
				b.redeliver.success(handlerName, msg.UUID)
				return produced, nil
			}
			if n := b.redeliver.failure(handlerName, msg.UUID); n > limit {
				b.logger.Error("message exceeded max redeliveries, dropping",
					"topic", topic,
					"key", msg.Metadata.Get(eventbus.MetaMessageKey),
					"message_id", msg.UUID,
					"redeliveries", n-1,
					"max_redeliveries", limit,
				)
				return nil, nil
			}
			return produced, err
		}
	}
}
