package lynx

import (
	"context"
	"gocloud.dev/server/health"
)

type Component interface {
	Name() string
	Init(lx Lynx) error
	Start(ctx context.Context) error
	Stop(ctx context.Context)
}

type ComponentProducer interface {
	Component() Component
	Option() ProduceOption
}

type ProduceOption struct {
	Instances int `json:"instances"` // 实例数
}

func (o *ProduceOption) ensureDefaults() {
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
