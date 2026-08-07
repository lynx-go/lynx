// Package boot 提供基于 Wire 依赖注入的应用引导，把依赖图中的
// 服务与 hooks 批量注册进应用。
package boot

import (
	"github.com/lynx-go/lynx"
)

// OnStartHooks 是一组启动钩子函数。
// 独立命名类型用于 Wire 依赖注入时区分启动钩子与停止钩子。
type OnStartHooks []lynx.HookFunc

// OnStopHooks 是一组停止钩子函数。
type OnStopHooks []lynx.HookFunc

// Bootstrap 聚合应用启动所需的钩子函数、服务与服务工厂。
type Bootstrap struct {
	StartHooks       OnStartHooks
	StopHooks        OnStopHooks
	Services         []lynx.Service
	ServiceFactories []lynx.ServiceFactory
}

// New 创建 Bootstrap 实例。
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

// Bind 将 Bootstrap 中的钩子函数、服务与服务工厂注册到 Lynx 应用。
// 注册阶段产生的错误（如服务 Init 失败）由 app.Run() 统一返回。
func (b *Bootstrap) Bind(app lynx.App) {
	app.OnStart(b.StartHooks...)
	app.OnStop(b.StopHooks...)
	app.Register(b.Services...)
	app.RegisterFactories(b.ServiceFactories...)
}
