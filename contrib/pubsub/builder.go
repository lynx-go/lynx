package pubsub

import (
	"fmt"

	"github.com/lynx-go/lynx"
)

// Bundle 是配置驱动装配的结果：Broker 与需要随应用注册的 Transports。
type Bundle struct {
	Broker     Broker
	Transports []Transport
}

// Components 返回应注册的全部组件（Transports + Broker），供 app.Register 使用。
func (b *Bundle) Components() []lynx.Component {
	comps := make([]lynx.Component, 0, len(b.Transports)+1)
	for _, t := range b.Transports {
		comps = append(comps, t)
	}
	return append(comps, b.Broker)
}

// NewFromConfig 从配置装配消息组件：
//   - "pubsub" 段 routes（逻辑 topic → {transport, key}）逐条应用 RouteKey，
//     引用未提供的 transport 标识时报错；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport 兼作默认回退；未提供时内置创建一个
//     内存 Transport 作为默认回退并纳入 Transports；
//   - map 中的字面 nil 值条目被防御性跳过；具体类型 nil 指针赋给接口
//     形成的 typed nil 无法在此检测，调用方须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (*Bundle, error) {
	var routesCfg struct {
		Routes map[string]struct {
			Transport string
			Key       string
		}
	}
	if err := cfg.UnmarshalKey("pubsub", &routesCfg); err != nil {
		return nil, err
	}

	// 默认回退：优先复用调用方提供的 "memory"，否则内置创建。
	memT, hasMemory := transports["memory"]
	if !hasMemory || memT == nil {
		memT = NewMemoryTransport()
	}
	opts := Options{DefaultTransport: memT}

	// 自动路由 transports 与注册列表；字面 nil 防御性跳过；memory 仅作
	// 默认回退（Topics() 为 nil），不重复进入自动路由表。
	registered := make([]Transport, 0, len(transports)+1)
	for name, t := range transports {
		if t == nil {
			continue
		}
		if name == "memory" {
			registered = append(registered, t)
			continue
		}
		opts.Transports = append(opts.Transports, t)
		registered = append(registered, t)
	}
	if !hasMemory || memT == nil {
		registered = append(registered, memT)
	}

	// 路由解析表：memory 标识始终可用（未提供时指向内置默认）。
	resolve := make(map[string]Transport, len(transports)+1)
	for name, t := range transports {
		if t != nil {
			resolve[name] = t
		}
	}
	if _, ok := resolve["memory"]; !ok {
		resolve["memory"] = memT
	}

	broker := NewBroker(opts)
	for topic, route := range routesCfg.Routes {
		t, ok := resolve[route.Transport]
		if !ok {
			return nil, fmt.Errorf("pubsub: route %q references unknown transport %q", topic, route.Transport)
		}
		if route.Key == "" {
			route.Key = topic
		}
		broker.RouteKey(topic, t, route.Key)
	}
	return &Bundle{Broker: broker, Transports: registered}, nil
}
