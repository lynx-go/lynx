package lynx

import (
	"context"

	"gocloud.dev/server/health"
)

// LifecycleManaged 定义组件的生命周期管理接口：初始化、启动与停止。
// Stop 的调用契约（P1-5）：必须容忍在 Start 之前被调用——Init 成功但
// Start 未执行（或 Start 已失败）时，框架会逆序调用 Stop 做资源清理。
// 实现不得假设 Start 必然先于 Stop。
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
	if o.Instances < 1 {
		o.Instances = 1
	}
}

// ServerLike 是同时具备健康检查能力的服务类组件。
type ServerLike interface {
	health.Checker
	Component
}

// HealthCheckFunc 返回应用当前的健康检查器列表。
type HealthCheckFunc func() []health.Checker
