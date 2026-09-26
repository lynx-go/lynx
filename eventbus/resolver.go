package eventbus

import (
	"time"

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

// ResolvedSubscription 是一次订阅的全部有效配置（Resolver 的唯一解析产物）：
// Topic 默认值、调用级选项与 Options.Topics / 全局默认合并为不可变值，Bus
// 实现只消费结果，投递路径不再重复解析（对齐 CORE-03 解码器订阅时解析）。
type ResolvedSubscription struct {
	// HandlerName 是有效 handler 名：SubscribeOptions.HandlerName 为空时回退为 topic。
	HandlerName string
	// MaxInFlight 是订阅级在途上限（0 = 后端默认；负数 = 不限制），见 SubscribeOptions。
	MaxInFlight int
	// Retry 是解析后的重试策略（高→低：调用/Topic 携带值 > Topics[t].Retry > 全局 > 默认 3 次）。
	Retry RetryOptions
	// HandlerTimeout 是 handler 单次尝试上限（0 = 不限制）。负值（调用级或
	// 主题级）= 显式禁用，解析时归一为 0。
	HandlerTimeout time.Duration
	// AutoAck / ContinueOnError 是调用级与 Topics[t] 的并集（只可能被打开）。
	AutoAck         bool
	ContinueOnError bool
}

// ResolveSubscription 把一次订阅的输入解析为唯一有效值（规则见各字段注释）：
//   - HandlerName 空 → topic；
//   - MaxInFlight / AutoAck / ContinueOnError：显式调用/Topic 携带值优先，
//     空缺由 Options.Topics[t] 填充；
//   - Retry / HandlerTimeout：调用值 > Topics[t] > 全局 > 默认（超时默认
//     不限制）；HandlerTimeout 负值 = 显式禁用。
func (r *Resolver) ResolveSubscription(topic string, o SubscribeOptions) ResolvedSubscription {
	res := ResolvedSubscription{
		HandlerName:     o.HandlerName,
		MaxInFlight:     o.MaxInFlight,
		AutoAck:         o.AutoAck,
		ContinueOnError: o.ContinueOnError,
	}
	if res.HandlerName == "" {
		res.HandlerName = topic
	}
	cfg, hasCfg := r.opts.Topics[topic]
	if res.MaxInFlight == 0 {
		res.MaxInFlight = cfg.MaxInFlight
	}
	if !res.AutoAck && hasCfg {
		res.AutoAck = cfg.AutoAck
	}
	if !res.ContinueOnError && hasCfg {
		res.ContinueOnError = cfg.ContinueOnError
	}

	// Retry：调用/Topic 携带值 > Topics[t].Retry > 全局 > 默认 3 次。
	switch {
	case o.Retry != nil:
		res.Retry = *o.Retry
	case hasCfg && cfg.Retry != nil:
		res.Retry = *cfg.Retry
	case r.opts.Retry != nil:
		res.Retry = *r.opts.Retry
	default:
		res.Retry = RetryOptions{MaxRetries: 3}
	}

	// HandlerTimeout：调用值 > Topics[t] > 全局；负值 = 显式禁用（归一 0）。
	switch {
	case o.HandlerTimeout != 0:
		if o.HandlerTimeout > 0 {
			res.HandlerTimeout = o.HandlerTimeout
		}
	case hasCfg && cfg.HandlerTimeout != 0:
		if cfg.HandlerTimeout > 0 {
			res.HandlerTimeout = cfg.HandlerTimeout
		}
	case r.opts.HandlerTimeout > 0:
		res.HandlerTimeout = r.opts.HandlerTimeout
	}
	return res
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
