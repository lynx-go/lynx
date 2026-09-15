// Package boot 提供基于 Wire 依赖注入的应用引导，把依赖图中的
// 服务与 hooks 批量注册进应用。
package boot

import (
	"github.com/lynx-go/lynx"
)

// PreStartHooks 是一组启动前钩子函数（对应 app.OnPreStart）。
// 独立命名类型用于 Wire 依赖注入时区分各生命周期阶段的钩子。
type PreStartHooks []lynx.HookFunc

// DrainHooks 是一组排水钩子函数（对应 app.OnDrain）：关停时
// drainChecker 置位后与 DrainTimeout 窗口睡眠并发执行（如从服务目录
// 注销），窗口即钩子总预算（启用排水须设置 WithDrainTimeout）。
type DrainHooks []lynx.HookFunc

// PreStopHooks 是一组停止前钩子函数（对应 app.OnPreStop）：先于服务
// Stop 执行，此时服务仍在服务在途请求。
type PreStopHooks []lynx.HookFunc

// PostStopHooks 是一组收尾清理钩子（对应 app.OnPostStop）：所有服务
// 与总线停止之后、Run 返回前逆序执行，总预算 CleanupTimeout。
// 典型来源是 Wire injector 返回的 cleanup 函数（关闭 DB/Redis 连接池）。
type PostStopHooks []lynx.CleanupFunc

// Bootstrap 聚合应用启动所需的钩子函数、服务与服务工厂。
type Bootstrap struct {
	PreStartHooks    PreStartHooks
	DrainHooks       DrainHooks
	PreStopHooks     PreStopHooks
	PostStopHooks    PostStopHooks
	Services         []lynx.Service
	ServiceFactories []lynx.ServiceFactory
}

// New 创建 Bootstrap 实例。参数顺序与字段声明顺序一致。
// v1.10.0 起不再保留历史参数顺序（drains 曾因 Wire injector 兼容被
// 挤成 WithDrainHooks setter）：本版本为不兼容重命名版本，全部
// injector 需重新生成，顺带修正了该顺序取舍。
func New(
	preStarts PreStartHooks,
	drains DrainHooks,
	preStops PreStopHooks,
	postStops PostStopHooks,
	services []lynx.Service,
	serviceFactories []lynx.ServiceFactory,
) *Bootstrap {
	return &Bootstrap{
		PreStartHooks:    preStarts,
		DrainHooks:       drains,
		PreStopHooks:     preStops,
		PostStopHooks:    postStops,
		Services:         services,
		ServiceFactories: serviceFactories,
	}
}

// Apply 将 Bootstrap 中的钩子函数、服务与服务工厂注册到 Lynx 应用。
// 注册阶段产生的错误（如服务 Init 失败）由 app.Run() 统一返回。
// v1.11.0 前名为 Bind：与 wire.Bind（接口绑定实现）及配置域的
// BindEnv/BindPFlags 撞名，且「b.Bind(app)」读作反向绑定，故更名。
func (b *Bootstrap) Apply(app lynx.App) {
	app.OnPreStart(b.PreStartHooks...)
	app.OnDrain(b.DrainHooks...)
	app.OnPreStop(b.PreStopHooks...)
	app.OnPostStop(b.PostStopHooks...)
	app.Register(b.Services...)
	app.RegisterFactories(b.ServiceFactories...)
}
