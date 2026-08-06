package pubsub

import (
	"fmt"

	"github.com/lynx-go/lynx"
)

// NewFromConfig 从配置装配 Broker：
//   - "pubsub" 段 routes（逻辑 topic → {transport, key}）逐条应用 RouteKey，
//     引用未提供的 transport 标识时报错；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport（提供且非 nil 时）兼作默认回退——未路由
//     的 topic 走它；不提供则无默认回退，未路由 topic 发布报错；
//   - 不创建任何 transport：kafka 与 memory 一律由调用方创建并注册
//     （生命周期归属应用）；
//   - map 中的字面 nil 值条目被防御性跳过；kafka 未启用的过滤由调用方
//     完成（示例 `if kafkaT != nil` 写法）。注意：具体类型 nil 指针赋给
//     Transport 接口（typed nil）无法在此检测，调用方必须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (Broker, error) {
	var routesCfg struct {
		Routes map[string]struct {
			Transport string
			Key       string
		}
	}
	if err := cfg.UnmarshalKey("pubsub", &routesCfg); err != nil {
		return nil, err
	}

	opts := Options{}
	for name, t := range transports {
		if t == nil {
			continue // 字面 nil 防御性跳过
		}
		opts.Transports = append(opts.Transports, t)
		if name == "memory" {
			opts.DefaultTransport = t
		}
	}

	broker := NewBroker(opts)
	for topic, route := range routesCfg.Routes {
		t, ok := transports[route.Transport]
		if !ok || t == nil {
			return nil, fmt.Errorf("pubsub: route %q references unknown transport %q", topic, route.Transport)
		}
		if route.Key == "" {
			route.Key = topic
		}
		broker.RouteKey(topic, t, route.Key)
	}
	return broker, nil
}
