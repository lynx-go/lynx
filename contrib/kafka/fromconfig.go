package kafka

import "github.com/lynx-go/lynx"

// NewFromConfig 从配置 "kafka" 段加载 Options 并创建 Transport。
// 段缺失或为空（无任何 topic）时返回 (nil, nil)，表示 Kafka 未启用；
// 调用方据此决定是否注册。
func NewFromConfig(cfg lynx.Config) (*Transport, error) {
	var opts Options
	if err := cfg.UnmarshalKey("kafka", &opts); err != nil {
		return nil, err
	}
	if len(opts.Topics) == 0 {
		return nil, nil
	}
	return NewTransport(opts)
}
