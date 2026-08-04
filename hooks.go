package lynx

import "context"

type hookOptions struct {
	onStarts          []HookFunc
	onStops           []HookFunc
	components        []Component
	componentBuilders []ComponentBuilder
}

// HookOption 用于向应用注册 hooks 与组件的选项函数。
type HookOption func(*hookOptions)

// OnStart 注册应用启动阶段执行的钩子函数。
func OnStart(fns ...HookFunc) HookOption {
	return func(options *hookOptions) {
		options.onStarts = append(options.onStarts, fns...)
	}
}

// OnStop 注册应用停止阶段执行的钩子函数。
func OnStop(fns ...HookFunc) HookOption {
	return func(options *hookOptions) {
		options.onStops = append(options.onStops, fns...)
	}
}

// Components 注册需要由应用托管生命周期的组件实例。
func Components(components ...Component) HookOption {
	return func(options *hookOptions) {
		options.components = append(options.components, components...)
	}
}

// ComponentBuilders 注册需要由应用托管生命周期的组件构建器。
func ComponentBuilders(builders ...ComponentBuilder) HookOption {
	return func(options *hookOptions) {
		options.componentBuilders = append(options.componentBuilders, builders...)
	}
}

// Hooks 定义启动/停止钩子函数的注册接口。
type Hooks interface {
	OnStart(fns ...HookFunc)
	OnStop(fns ...HookFunc)
}

// OnStartHooks 是一组启动钩子函数。
type OnStartHooks []HookFunc

// OnStopHooks 是一组停止钩子函数。
type OnStopHooks []HookFunc

// HookFunc 是应用生命周期钩子函数，返回错误时视为钩子执行失败。
type HookFunc func(ctx context.Context) error

type hooks struct {
	onStarts []HookFunc
	onStops  []HookFunc
}

func (hooks *hooks) OnStart(fns ...HookFunc) {
	hooks.onStarts = append(hooks.onStarts, fns...)
}

func (hooks *hooks) OnStop(fns ...HookFunc) {
	hooks.onStops = append(hooks.onStops, fns...)
}

var _ Hooks = new(hooks)
