package eventbus

import (
	"time"
)

// RetryOptions 配置 handler 失败重试。
type RetryOptions struct {
	MaxRetries int
	Backoff    time.Duration
}

// LogMessageOptions 控制收发日志（debug 级）。
type LogMessageOptions struct {
	Publish   bool
	Subscribe bool
}

// TopicConfig 是单主题的完整选项（用于 BusOptions.Topics；Topic[T] 的运行时视图亦由此生成）。
type TopicConfig struct {
	Group           string             `mapstructure:"group"`
	Instances       int                `mapstructure:"instances"`
	AutoAck         bool               `mapstructure:"auto_ack"`
	ContinueOnError bool               `mapstructure:"continue_on_error"`
	Retry           *RetryOptions      `mapstructure:"retry"`
	LogMessage      *LogMessageOptions `mapstructure:"log_message"`
	Marshaler       Marshaler          `mapstructure:"-"`
}

// Options 是 Bus 的构造选项，全部有默认值，开箱即零值可用。
type Options struct {
	// BufferSize 是内存 Bus 每订阅者的通道缓冲，默认 64。
	BufferSize int
	// Marshaler 是全局默认序列化器，nil 时为 JSON。
	Marshaler Marshaler
	// TopicMarshalers 按主题覆盖 Marshaler。
	TopicMarshalers map[string]Marshaler
	// Retry 是全局重试默认，nil 时为 {MaxRetries: 3}。
	Retry *RetryOptions
	// LogMessage 是全局收发日志默认。
	LogMessage *LogMessageOptions
	// Debug 控制底层详细日志（watermill 侧），默认 false。
	Debug bool
	// PropagateAttrs 是跨请求传播的日志属性白名单，nil 时为 {request_id,user_id}，
	// 非 nil 空切片表示关闭。
	PropagateAttrs []string
	// Topics 按主题的精细选项，Subscribe 时合并为默认值。
	Topics map[string]TopicConfig
	// Transports 参与自动路由的后端（内存 Bus 为空，Watermill Bus 由 contrib 注入）。
	Transports []Transport
	// DefaultTransport 回退后端。
	DefaultTransport Transport
}

// EnsureDefaults 填充默认值，幂等。
func (o *Options) EnsureDefaults() {
	if o.BufferSize <= 0 {
		o.BufferSize = 64
	}
	if o.Retry == nil {
		o.Retry = &RetryOptions{MaxRetries: 3}
	}
	if o.Topics == nil {
		o.Topics = map[string]TopicConfig{}
	}
	if o.TopicMarshalers == nil {
		o.TopicMarshalers = map[string]Marshaler{}
	}
}
