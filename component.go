package lynx

import (
	"context"

	"gocloud.dev/server/health"
)

type LifecycleManaged interface {
	Init(app Lynx) error
	Start(ctx context.Context) error
	Stop(ctx context.Context)
}

type Component interface {
	Name() string
	LifecycleManaged
}

type ComponentBuilder interface {
	Build() Component
	Options() BuildOptions
}

type BuildOptions struct {
	Instances int `json:"instances"` // 实例数
}

func (o *BuildOptions) ensureDefaults() {
	if o.Instances == 0 {
		o.Instances = 1
	}
}

type ComponentSet []Component

type ServerLike interface {
	health.Checker
	Component
}

type HealthCheckFunc func() []health.Checker
