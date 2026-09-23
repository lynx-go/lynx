package eventbus

import (
	"github.com/lynx-go/lynx/logging"
)

// Resolver 是 Bus 配置解析的唯一归属（导出供 contrib Bus 实现复用）：
// marshaler / retry / log-message / 传播键的查找与 Topic 级合并都在此，
// 适配器只消费结果。构造时补齐默认值；方法只读，并发安全。
type Resolver struct {
	opts Options
}

// NewResolver 从构造选项创建解析器（EnsureDefaults 在此完成，幂等）。
func NewResolver(opts Options) *Resolver {
	opts.EnsureDefaults()
	return &Resolver{opts: opts}
}

// MarshalerFor 返回主题序列化器（查找序：TopicMarshalers[t] → Topics[t].Marshaler → 全局 → JSON）。
func (r *Resolver) MarshalerFor(topic string) Marshaler {
	if m, ok := r.opts.TopicMarshalers[topic]; ok {
		return m
	}
	if cfg, ok := r.opts.Topics[topic]; ok && cfg.Marshaler != nil {
		return cfg.Marshaler
	}
	if r.opts.Marshaler != nil {
		return r.opts.Marshaler
	}
	return JSONMarshaler{}
}

// RetryFor 解析订阅的重试默认（高→低）：调用级 call（SubscribeOptions.Retry，
// Topic 携带值经 WithSubscribeRetry 注入）> Topics[t].Retry > 全局 > 默认 3 次。
func (r *Resolver) RetryFor(topic string, call *RetryOptions) RetryOptions {
	if call != nil {
		return *call
	}
	if cfg, ok := r.opts.Topics[topic]; ok && cfg.Retry != nil {
		return *cfg.Retry
	}
	if r.opts.Retry != nil {
		return *r.opts.Retry
	}
	return RetryOptions{MaxRetries: 3}
}

// LogMessageFor 返回 topic 的收发日志选项（Topics[t].LogMessage > 全局）。
func (r *Resolver) LogMessageFor(topic string) LogMessageOptions {
	if cfg, ok := r.opts.Topics[topic]; ok && cfg.LogMessage != nil {
		return *cfg.LogMessage
	}
	if r.opts.LogMessage != nil {
		return *r.opts.LogMessage
	}
	return LogMessageOptions{}
}

// PropagateKeys 返回跨请求传播的日志属性白名单。
// nil 表示默认 {request_id, user_id}；非 nil 空切片表示关闭传播。
func (r *Resolver) PropagateKeys() []string {
	if r.opts.PropagateAttrs != nil {
		return r.opts.PropagateAttrs
	}
	return []string{logging.FieldRequestID, logging.FieldUserID}
}

// ApplyTopicDefaults 将 Options.Topics[t] 的订阅默认合并进订阅选项：
// 显式调用选项优先（只填空缺），覆盖 Group / Instances / AutoAck /
// ContinueOnError 四项；Retry 经 RetryFor 在投递执行时解析。
func (r *Resolver) ApplyTopicDefaults(topic string, o *SubscribeOptions) {
	cfg, ok := r.opts.Topics[topic]
	if !ok {
		return
	}
	if o.Group == "" {
		o.Group = cfg.Group
	}
	if o.Instances == 0 {
		o.Instances = cfg.Instances
	}
	if !o.AutoAck && cfg.AutoAck {
		o.AutoAck = true
	}
	if !o.ContinueOnError && cfg.ContinueOnError {
		o.ContinueOnError = true
	}
}
