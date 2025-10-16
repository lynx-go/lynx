package lynx

import (
	"context"

	"gocloud.dev/server/health"
)

type Component interface {
	Name() string
	Init(app Lynx) error
	Start(ctx context.Context) error
	Stop(ctx context.Context)
}

type ComponentFactory interface {
	Component() Component
	Option() FactoryOption
}

type FactoryOption struct {
	Instances int `json:"instances"` // 实例数
}

func (o *FactoryOption) ensureDefaults() {
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
