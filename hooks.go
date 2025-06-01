package lynx

import "context"

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
