package lynx

import (
	"context"
	"log/slog"
)

// Env 是框架提供给组件的运行环境：组件的 Init 只依赖 Env，不依赖完整的
// App（App 是 Env 的超集）。组件因此不需要为测试实现 App 的其余方法。
type Env interface {
	// Context 获取应用上下文
	Context() context.Context
	// Config 获取配置实例（默认由 *viper.Viper 适配实现）
	Config() Config
	// Logger 获取 logger
	Logger(kwargs ...any) *slog.Logger
	// HealthCheckers 返回当前已注册的健康检查器快照（注册时收集）。
	HealthCheckers() []Checker
	// Close 关闭应用实例（如一次性命令执行完毕）。
	Close()
}

// Lifecycle 定义组件的生命周期管理接口：初始化、启动与停止。
// Stop 的调用契约：必须容忍在 Start 之前被调用——Init 成功但
// Start 未执行（或 Start 已失败）时，框架会逆序调用 Stop 做资源清理。
// 实现不得假设 Start 必然先于 Stop。Stop 返回的错误会被框架收集，
// 与 OnStop 钩子错误一起由 Run() 统一上抛给调用方。
type Lifecycle interface {
	Init(env Env) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Component 是可由 App 托管生命周期的命名组件。
type Component interface {
	Name() string
	Lifecycle
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
	Checker
	Component
}

// HealthCheckersFunc 是健康检查器的取值函数，用于 server 组件的健康检查
// 配置。方法值 app.HealthCheckers 天然匹配该签名。
type HealthCheckersFunc func() []Checker
