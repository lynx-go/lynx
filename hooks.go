package lynx

import "context"

type hookOptions struct {
	onStarts          []HookFunc
	onStops           []HookFunc
	components        []Component
	componentBuilders []ComponentBuilder
}

type HookOption func(*hookOptions)

func WithOnStart(fns ...HookFunc) HookOption {
	return func(options *hookOptions) {
		options.onStarts = append(options.onStarts, fns...)
	}
}

func WithOnStop(fns ...HookFunc) HookOption {
	return func(options *hookOptions) {
		options.onStops = append(options.onStops, fns...)
	}
}

func WithComponent(components ...Component) HookOption {
	return func(options *hookOptions) {
		options.components = append(options.components, components...)
	}
}

func WithComponentBuilder(builders ...ComponentBuilder) HookOption {
	return func(options *hookOptions) {
		options.componentBuilders = append(options.componentBuilders, builders...)
	}
}

type Hooks interface {
	OnStart(fns ...HookFunc)
	OnStop(fns ...HookFunc)
}
type OnStartHooks []HookFunc
type OnStopHooks []HookFunc

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
