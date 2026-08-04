package boot

import (
	"github.com/lynx-go/lynx"
)

// Bootstrap 聚合应用启动所需的钩子函数、组件与组件构建器。
type Bootstrap struct {
	StartHooks              []lynx.HookFunc
	StopHooks               []lynx.HookFunc
	Components              []lynx.Component
	ComponentBuilders       []lynx.ComponentBuilder
	ComponentBuilderSetFunc lynx.ComponentBuilderSetFunc
}

// New 创建 Bootstrap 实例。
func New(
	onStarts lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentBuilders []lynx.ComponentBuilder,
	componentBuilderSetFunc lynx.ComponentBuilderSetFunc,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:              onStarts,
		StopHooks:               onStops,
		Components:              components,
		ComponentBuilders:       componentBuilders,
		ComponentBuilderSetFunc: componentBuilderSetFunc,
	}
}

// Bind 将 Bootstrap 中的钩子函数、组件与组件构建器绑定到 Lynx 应用。
func (b *Bootstrap) Bind(app lynx.App) error {
	if err := app.Hooks(lynx.OnStart(b.StartHooks...)); err != nil {
		return err
	}
	if err := app.Hooks(lynx.OnStop(b.StopHooks...)); err != nil {
		return err
	}
	if err := app.Hooks(lynx.Components(b.Components...)); err != nil {
		return err
	}

	if err := app.Hooks(lynx.ComponentBuilders(b.ComponentBuilders...)); err != nil {
		return err
	}
	if b.ComponentBuilderSetFunc != nil {
		if err := app.Hooks(lynx.ComponentBuilders(b.ComponentBuilderSetFunc()...)); err != nil {
			return err
		}
	}
	return nil
}
