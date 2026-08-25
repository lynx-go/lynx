// Package boot 提供基于 Wire 依赖注入的应用引导，把依赖图中的
// 服务与 hooks 批量注册进应用。
package boot

import (
	"github.com/lynx-go/lynx"
)

// OnStartHooks 是一组启动钩子函数。
// 独立命名类型用于 Wire 依赖注入时区分启动钩子与停止钩子。
type OnStartHooks []lynx.HookFunc

// OnDrainHooks 是一组排水钩子函数：关停时 drainChecker 置位后与
// DrainTimeout 睡眠并发执行（如从服务目录注销），预算为 DrainHookTimeout。
type OnDrainHooks []lynx.HookFunc

// OnStopHooks 是一组停止钩子函数。
type OnStopHooks []lynx.HookFunc

// Bootstrap 聚合应用启动所需的钩子函数、服务与服务工厂。
type Bootstrap struct {
	StartHooks       OnStartHooks
	DrainHooks       OnDrainHooks
	StopHooks        OnStopHooks
	Services         []lynx.Service
	ServiceFactories []lynx.ServiceFactory
}

// New 创建 Bootstrap 实例。
// 参数顺序（onStarts、onStops、services、serviceFactories）与 Bootstrap
// 字段声明顺序不同（中间隔着 DrainHooks）——这是历史固定的 Wire 兼容
// 取舍：签名被既有 wire 生成的 injector 代码引用，调整参数顺序会破坏
// 全部 injector 的可编译性；后加入的排水钩子因此走可选的
// WithDrainHooks setter（不改 New 签名），而非插入新参数。
func New(
	onStarts OnStartHooks,
	onStops OnStopHooks,
	services []lynx.Service,
	serviceFactories []lynx.ServiceFactory,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:       onStarts,
		StopHooks:        onStops,
		Services:         services,
		ServiceFactories: serviceFactories,
	}
}

// WithDrainHooks 设置排水钩子（可选 setter，不改 New 签名以保持
// 既有 Wire injector 可编译），返回 Bootstrap 自身以便链式调用。
func (b *Bootstrap) WithDrainHooks(h OnDrainHooks) *Bootstrap {
	b.DrainHooks = h
	return b
}

// Bind 将 Bootstrap 中的钩子函数、服务与服务工厂注册到 Lynx 应用。
// 注册阶段产生的错误（如服务 Init 失败）由 app.Run() 统一返回。
func (b *Bootstrap) Bind(app lynx.App) {
	app.OnStart(b.StartHooks...)
	app.OnDrain(b.DrainHooks...)
	app.OnStop(b.StopHooks...)
	app.Register(b.Services...)
	app.RegisterFactories(b.ServiceFactories...)
}
