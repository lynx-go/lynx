package lynx

import (
	"context"

	"gocloud.dev/server/health"
)

// LifecycleManaged 定义组件的生命周期管理接口：初始化、启动与停止。
type LifecycleManaged interface {
	Init(app App) error
	Start(ctx context.Context) error
	Stop(ctx context.Context)
}

// Component 是可由 App 托管生命周期的命名组件。
type Component interface {
	Name() string
	LifecycleManaged
}

// ComponentBuilderSet 是一组组件构建器。
type ComponentBuilderSet []ComponentBuilder

// ComponentBuilderSetFunc 返回一组组件构建器，常用于按配置动态构建组件。
type ComponentBuilderSetFunc func() ComponentBuilderSet

// ComponentBuilder 按 BuildOptions 描述的方式构建组件实例。
type ComponentBuilder interface {
	Build() Component
	Options() BuildOptions
}

// BuildOptions 描述组件构建参数。
type BuildOptions struct {
	Instances int `json:"instances"` // 实例数
}

func (o *BuildOptions) ensureDefaults() {
	if o.Instances == 0 {
		o.Instances = 1
	}
}

// ComponentSet 是一组组件实例。
type ComponentSet []Component

// ServerLike 是同时具备健康检查能力的服务类组件。
type ServerLike interface {
	health.Checker
	Component
}

// HealthCheckFunc 返回应用当前的健康检查器列表。
type HealthCheckFunc func() []health.Checker
