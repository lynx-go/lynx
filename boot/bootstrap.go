package boot

import (
	"github.com/lynx-go/lynx"
)

type Bootstrap struct {
	StartHooks        []lynx.HookFunc
	StopHooks         []lynx.HookFunc
	Components        []lynx.Component
	ComponentBuilders []lynx.ComponentBuilder
}

func New(
	onStars lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentBuilders []lynx.ComponentBuilder,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:        onStars,
		StopHooks:         onStops,
		Components:        components,
		ComponentBuilders: componentBuilders,
	}
}

func (b *Bootstrap) Build(fl lynx.Lynx) error {
	if err := fl.Hook(lynx.OnStart(b.StartHooks...)); err != nil {
		return err
	}
	if err := fl.Hook(lynx.OnStop(b.StopHooks...)); err != nil {
		return err
	}
	if err := fl.Hook(lynx.WithComponent(b.Components...)); err != nil {
		return err
	}

	if err := fl.Hook(lynx.WithComponentBuilder(b.ComponentBuilders...)); err != nil {
		return err
	}
	return nil
}
