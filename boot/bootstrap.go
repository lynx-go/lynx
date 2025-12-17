package boot

import (
	"github.com/lynx-go/lynx"
)

type Bootstrap struct {
	StartHooks              []lynx.HookFunc
	StopHooks               []lynx.HookFunc
	Components              []lynx.Component
	ComponentBuilders       []lynx.ComponentBuilder
	ComponentBuilderSetFunc lynx.ComponentBuilderSetFunc
}

func New(
	onStars lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentBuilders []lynx.ComponentBuilder,
	componentBuilderSetFunc lynx.ComponentBuilderSetFunc,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:              onStars,
		StopHooks:               onStops,
		Components:              components,
		ComponentBuilders:       componentBuilders,
		ComponentBuilderSetFunc: componentBuilderSetFunc,
	}
}

func (b *Bootstrap) Build(app lynx.Lynx) error {
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
	if err := app.Hooks(lynx.ComponentBuilders(b.ComponentBuilderSetFunc()...)); err != nil {
		return err
	}
	return nil
}
