package kafka

import (
	"github.com/lynx-go/lynx"
	lynxwatermill "github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

// NewFromConfig 从配置 "kafka" 段加载 Options 并创建 Transport。
// 段缺失或为空（无任何 topic）时返回 (nil, nil)，表示 Kafka 未启用；
// 调用方据此决定是否注册。段内字段类型非法（如 brokers 写成标量）时
// 返回错误——类型预检垫片已由 lynx.WithStrictTypes 在解码器层统一实现。
func NewFromConfig(cfg lynx.Config) (*Transport, error) {
	var opts Options
	if err := cfg.UnmarshalKey("kafka", &opts, lynx.WithStrictTypes()); err != nil {
		return nil, err
	}
	if len(opts.Topics) == 0 {
		return nil, nil
	}
	return NewTransport(opts)
}

// NewBusFromConfig 从配置装配 Watermill Bus（kafka 版推荐装配入口）：
//   - 始终提供 "memory" transport，兼作 DefaultTransport——承接 lynx.*
//     生命周期事件与未 route 的 topic；
//   - "kafka" 段启用时创建 Transport，加为 "kafka" route 并作为配套服务
//     返回（框架托管生命周期与健康聚合）；段缺失或为空时为纯内存总线
//     （配置即开关）；
//   - 返回值签名与 lynx.WithBusProvider 一致，可直接传入。
//
// 需要自定义 transport 集合（额外后端、替换 memory）时走手工装配：
// watermill.NewFromConfig(cfg, transports)，见 contrib/watermill README。
func NewBusFromConfig(cfg lynx.Config) (eventbus.Bus, []lynx.Service, error) {
	kafkaT, err := NewFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	transports := map[string]eventbus.Transport{"memory": lynxwatermill.NewMemoryTransport()}
	var svcs []lynx.Service
	if kafkaT != nil {
		transports["kafka"] = kafkaT
		svcs = append(svcs, kafkaT) // Transport 生命周期由框架托管
	}
	bus, err := lynxwatermill.NewFromConfig(cfg, transports)
	if err != nil {
		return nil, nil, err
	}
	return bus, svcs, nil
}
