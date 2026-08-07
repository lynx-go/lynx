package pubsub

import (
	"fmt"
	"time"

	"github.com/lynx-go/lynx"
)

// pubsubConfig 是 "pubsub" 段的配置结构（NewFromConfig 使用）。
type pubsubConfig struct {
	// Retry 是全局 handler 重试默认值；事件未单独配置 retry 时生效。
	Retry *retryConfig `mapstructure:"retry"`
	// Events 按逻辑 topic（业务事件名）配置路由与事件级选项。
	Events map[string]eventConfig `mapstructure:"events"`
}

// eventConfig 是一个逻辑 topic 的配置：route 显式路由，其余字段是事件级
// 选项（Subscribe 默认值/收发日志/重试覆盖），后续可按需在同级扩展。
type eventConfig struct {
	// Route 显式路由 {transport, key}；缺省时走自动路由/默认回退。
	Route *routeConfig `mapstructure:"route"`
	// LogMessage 对该事件的发布与消费输出 debug 日志。
	LogMessage bool `mapstructure:"log_message"`
	// AutoAck/ContinueOnError/Group/Instances 作为 Subscribe 的默认选项
	//（显式 SubscribeOption 优先）。
	AutoAck         bool   `mapstructure:"auto_ack"`
	ContinueOnError bool   `mapstructure:"continue_on_error"`
	Group           string `mapstructure:"group"`
	Instances       int    `mapstructure:"instances"`
	// Retry 覆盖全局重试；缺省沿用 pubsub.retry（再缺省 {MaxRetries: 3}）。
	Retry *retryConfig `mapstructure:"retry"`
}

// routeConfig 是事件的路由配置：transport 是后端标识（如 kafka/memory），
// key 是调用 transport 时的主题名（对 kafka 即 kafka 段配置的逻辑 key），
// 缺省与逻辑 topic 同名。
type routeConfig struct {
	Transport string `mapstructure:"transport"`
	Key       string `mapstructure:"key"`
}

// retryConfig 是 handler 重试配置。
type retryConfig struct {
	MaxRetries int           `mapstructure:"max_retries"`
	Backoff    time.Duration `mapstructure:"backoff"`
}

func (r *retryConfig) toOptions() *RetryOptions {
	if r == nil {
		return nil
	}
	return &RetryOptions{MaxRetries: r.MaxRetries, Backoff: r.Backoff}
}

// NewFromConfig 从配置装配 Broker：
//   - "pubsub" 段 events（逻辑 topic → 事件配置）：route 指定 {transport, key}
//     时逐条应用 RouteKey，引用未提供的 transport 标识时报错；route 缺省的
//     topic 走自动路由/默认回退；
//   - events 的事件级选项（log_message/auto_ack/continue_on_error/group/
//     instances/retry）作为 Broker 的 Options.Events：Subscribe 合并为默认
//     订阅选项，发布/消费按 log_message 输出日志，retry 覆盖全局重试；
//   - "pubsub" 段 retry 配置全局 handler 重试默认值（事件未单独配置时生效）；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport（提供且非 nil 时）兼作默认回退——未路由
//     的 topic 走它；不提供则无默认回退，未路由 topic 发布报错；
//   - 不创建任何 transport：kafka 与 memory 一律由调用方创建并注册
//     （生命周期归属应用）；
//   - map 中的字面 nil 值条目被防御性跳过；kafka 未启用的过滤由调用方
//     完成（示例 `if kafkaT != nil` 写法）。注意：具体类型 nil 指针赋给
//     Transport 接口（typed nil）无法在此检测，调用方必须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (Broker, error) {
	var cfgPubsub pubsubConfig
	if err := cfg.UnmarshalKey("pubsub", &cfgPubsub); err != nil {
		return nil, err
	}

	opts := Options{Retry: cfgPubsub.Retry.toOptions()}
	for name, t := range transports {
		if t == nil {
			continue // 字面 nil 防御性跳过
		}
		opts.Transports = append(opts.Transports, t)
		if name == "memory" {
			opts.DefaultTransport = t
		}
	}
	if len(cfgPubsub.Events) > 0 {
		opts.Events = make(map[string]EventOptions, len(cfgPubsub.Events))
		for topic, ev := range cfgPubsub.Events {
			opts.Events[topic] = EventOptions{
				LogMessage:      ev.LogMessage,
				AutoAck:         ev.AutoAck,
				ContinueOnError: ev.ContinueOnError,
				Group:           ev.Group,
				Instances:       ev.Instances,
				Retry:           ev.Retry.toOptions(),
			}
		}
	}

	broker := NewBroker(opts)
	for topic, ev := range cfgPubsub.Events {
		if ev.Route == nil {
			continue // 无显式路由：自动路由/默认回退
		}
		t, ok := transports[ev.Route.Transport]
		if !ok || t == nil {
			return nil, fmt.Errorf("pubsub: route %q references unknown transport %q", topic, ev.Route.Transport)
		}
		key := ev.Route.Key
		if key == "" {
			key = topic
		}
		broker.RouteKey(topic, t, key)
	}
	return broker, nil
}
