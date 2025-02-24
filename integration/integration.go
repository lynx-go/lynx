package integration

import (
	"context"
)

type Func func(ctx context.Context) error

type Registrar struct {
	integrations []Integration
}

func (reg *Registrar) Integrations() []Integration {
	return reg.integrations
}

func (reg *Registrar) Register(hooks ...Integration) {
	reg.integrations = append(reg.integrations, hooks...)
}

func (reg *Registrar) OnStart(fns ...Func) {
	for _, fn := range fns {
		hook := &onStart{fn: fn}
		reg.integrations = append(reg.integrations, hook)
	}
}

func (reg *Registrar) OnStop(fns ...Func) {
	for _, fn := range fns {
		hook := &onStop{fn: fn}
		reg.integrations = append(reg.integrations, hook)
	}
}

type onStop struct {
	fn Func
}

func (h *onStop) Status() (int, error) {
	return 200, nil
}

func (h *onStop) Name() string {
	return "Stop"
}

func (h *onStop) Start(ctx context.Context) error {
	return nil
}

func (h *onStop) Stop(ctx context.Context) error {
	return h.fn(ctx)
}

var _ Integration = new(onStop)

type onStart struct {
	fn Func
}

func (h *onStart) Status() (int, error) {
	return 200, nil
}

func (h *onStart) Name() string {
	return "Start"
}

func (h *onStart) Start(ctx context.Context) error {
	return h.fn(ctx)
}

func (h *onStart) Stop(ctx context.Context) error {
	return nil
}

var _ Integration = new(onStart)

type Integration interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status() (int, error)
}
