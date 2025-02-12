package lifecycle

import (
	"context"
	"sync"
)

type Hook interface {
	Name() string
	OnStart(ctx context.Context) error
	OnStop(ctx context.Context)
	IgnoreCLI() bool
}

type Registry struct {
	hooks []Hook
	mutex sync.Mutex
}

func NewHooks() *Registry {
	return &Registry{
		hooks: make([]Hook, 0),
	}
}

func (h *Registry) append(hk Hook) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.hooks = append(h.hooks, hk)
}

func (h *Registry) OnStart(fns ...func(ctx context.Context) error) {
	for _, fn := range fns {
		hk := &onStart{fn: fn}
		h.append(hk)
	}
}

func (h *Registry) OnStop(fns ...func(ctx context.Context)) {
	for _, fn := range fns {
		hk := &onStop{fn: fn}
		h.append(hk)
	}
}

func (h *Registry) Service(svcs ...Service) {
	for _, s := range svcs {
		hk := ServiceToHook(s)
		h.append(hk)
	}
}

func (h *Registry) Hook(hooks ...Hook) {
	for _, hook := range hooks {
		h.append(hook)
	}
}

func (h *Registry) Range() []Hook {
	return h.hooks
}

type HookBase struct {
}

func (h *HookBase) Name() string {
	return "base"
}

func (h *HookBase) IgnoreCLI() bool {
	return false
}

func (h *HookBase) OnStart(ctx context.Context) error {
	return nil
}

func (h *HookBase) OnStop(ctx context.Context) {
}

var _ Hook = new(HookBase)

type onStart struct {
	fn func(ctx context.Context) error
}

func (o *onStart) Name() string {
	return "onStart"
}

func (o *onStart) OnStart(ctx context.Context) error {
	return o.fn(ctx)
}

func (o *onStart) OnStop(ctx context.Context) {
}

func (o *onStart) IgnoreCLI() bool {
	return false
}

var _ Hook = new(onStart)

var _ Hook = new(onStop)

type onStop struct {
	fn func(ctx context.Context)
}

func (o *onStop) Name() string {
	return "onStop"
}

func (o *onStop) OnStart(ctx context.Context) error {
	return nil
}

func (o *onStop) OnStop(ctx context.Context) {
	o.fn(ctx)
}

func (o *onStop) IgnoreCLI() bool {
	return false
}

type service struct {
	s Service
}

func (h *service) Name() string {
	return h.s.Name()
}

func (h *service) OnStart(ctx context.Context) error {
	return h.s.Start(ctx)
}

func (h *service) OnStop(ctx context.Context) {
	h.s.Stop(ctx)
}

func (h *service) IgnoreCLI() bool {
	return h.s.IgnoreCLI()
}

var _ Hook = new(service)

func ServiceToHook(s Service) Hook {
	return &service{s: s}
}
