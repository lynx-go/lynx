package boot

import (
	"github.com/lynx-go/lynx"
)

type Bootstrap struct {
	StartHooks         []lynx.HookFunc
	StopHooks          []lynx.HookFunc
	Components         []lynx.Component
	ComponentProducers []lynx.ComponentFactory
}

func NewBootstrap(
	onStars lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentProducers []lynx.ComponentFactory,
) *Bootstrap {
	return &Bootstrap{
		StartHooks:         onStars,
		StopHooks:          onStops,
		Components:         components,
		ComponentProducers: componentProducers,
	}
}

func (b *Bootstrap) Bind(fl lynx.Lynx) error {
	fl.Hooks().OnStart(b.StartHooks...)
	fl.Hooks().OnStop(b.StopHooks...)
	if err := fl.Register(b.Components...); err != nil {
		return err
	}

	if err := fl.RegisterFactory(b.ComponentProducers...); err != nil {
		return err
	}
	return nil
}
