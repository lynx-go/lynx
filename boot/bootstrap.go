package boot

import (
	"github.com/lynx-go/lynx"
)

type Bootstrap struct {
	StartHooks         []lynx.HookFunc
	StopHooks          []lynx.HookFunc
	Components         []lynx.Component
	ComponentProducers []lynx.ComponentProducer
}

func NewBootstrap(
	onStars lynx.OnStartHooks,
	onStops lynx.OnStopHooks,
	components []lynx.Component,
	componentProducers []lynx.ComponentProducer,
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
	if err := fl.Load(b.Components...); err != nil {
		return err
	}

	if err := fl.LoadFromProducer(b.ComponentProducers...); err != nil {
		return err
	}
	return nil
}
