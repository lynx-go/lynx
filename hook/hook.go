package hook

import (
	"context"
	"sync"
)

type Func func(ctx context.Context) error

type Hooks struct {
	mu    sync.Mutex
	hooks []Hook
}

func (reg *Hooks) Hooks() []Hook {
	return reg.hooks
}

func (reg *Hooks) Hook(hooks ...Hook) {
	reg.hooks = append(reg.hooks, hooks...)
}

func (reg *Hooks) OnStart(fns ...Func) {
	for _, fn := range fns {
		hook := &onStart{fn: fn}
		reg.hooks = append(reg.hooks, hook)
	}
}

func (reg *Hooks) OnStop(fns ...Func) {
	for _, fn := range fns {
		hook := &onStop{fn: fn}
		reg.hooks = append(reg.hooks, hook)
	}
}

type onStop struct {
	fn Func
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

var _ Hook = new(onStop)

type onStart struct {
	fn Func
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

var _ Hook = new(onStart)

type Hook interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}
