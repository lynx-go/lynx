package bootstrap

import (
	"github.com/lynx-go/lynx"
)

type Bootstrap struct {
	StartHooks         []lynx.HookFunc
	StopHooks          []lynx.HookFunc
	Components         []lynx.Component
	ComponentFactories []lynx.ComponentBuilder
}

func New(
	onStars lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentFactories []lynx.ComponentBuilder,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:         onStars,
		StopHooks:          onStops,
		Components:         components,
		ComponentFactories: componentFactories,
	}
}

func (b *Bootstrap) Build(fl lynx.Lynx) error {
	fl.OnStart(b.StartHooks...)
	fl.OnStop(b.StopHooks...)
	if err := fl.Hook(b.Components...); err != nil {
		return err
	}

	if err := fl.Builder(b.ComponentFactories...); err != nil {
		return err
	}
	return nil
}
