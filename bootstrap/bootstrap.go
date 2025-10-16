package bootstrap

import (
	"github.com/lynx-go/lynx"
)

type Bootstrap struct {
	StartHooks         []lynx.HookFunc
	StopHooks          []lynx.HookFunc
	Components         []lynx.Component
	ComponentFactories []lynx.ComponentFactory
}

func New(
	onStars lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentFactories []lynx.ComponentFactory,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:         onStars,
		StopHooks:          onStops,
		Components:         components,
		ComponentFactories: componentFactories,
	}
}

func (b *Bootstrap) Wire(fl lynx.Lynx) error {
	fl.Hooks().OnStart(b.StartHooks...)
	fl.Hooks().OnStop(b.StopHooks...)
	if err := fl.Register(b.Components...); err != nil {
		return err
	}

	if err := fl.RegisterFactory(b.ComponentFactories...); err != nil {
		return err
	}
	return nil
}
