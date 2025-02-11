package hook

import (
	"context"
	"sync"
)

type Hook interface {
	//OnInit()

	OnStart(ctx context.Context) error
	OnStop(ctx context.Context)
	IgnoreForCLI() bool
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

func (h *Registry) Register(hook Hook) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.hooks = append(h.hooks, hook)
}

func (h *Registry) Range() []Hook {
	return h.hooks
}

type HookBase struct {
}

func (h *HookBase) IgnoreForCLI() bool {
	return false
}

func (h *HookBase) OnStart(ctx context.Context) error {
	return nil
}

func (h *HookBase) OnStop(ctx context.Context) {
}

var _ Hook = new(HookBase)
