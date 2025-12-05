package bootstrap

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
	fl.OnStart(b.StartHooks...)
	fl.OnStop(b.StopHooks...)
	if err := fl.LoadComponents(b.Components...); err != nil {
		return err
	}

	if err := fl.LoadComponentBuilders(b.ComponentBuilders...); err != nil {
		return err
	}
	return nil
}
