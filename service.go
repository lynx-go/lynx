package lynx

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/eventbus"
)

// AppContext 是框架提供给服务的应用上下文：服务的 Init 只依赖 AppContext，
// 不依赖完整的 App（App 是 AppContext 的超集）。服务因此不需要为测试实现
// App 的其余方法。
type AppContext interface {
	// Context 获取应用上下文
	Context() context.Context
	// Config 获取配置实例（默认由 *viper.Viper 适配实现）
	Config() Config
	// Logger 获取 logger
	Logger(kwargs ...any) *slog.Logger
	// Bus 获取应用级消息总线（一等对象，开箱即为内存实现，可被 Watermill/Kafka 装饰）。
	Bus() eventbus.Bus
	// HealthCheckers 返回当前已注册的健康检查器快照（注册时收集）。
	HealthCheckers() []Checker
	// Close 关闭应用实例（如一次性命令执行完毕）。
	Close()
}

// Lifecycle 定义服务的生命周期管理接口：初始化、启动与停止。
// Stop 的调用契约：必须容忍在 Start 之前被调用——Init 成功但
// Start 未执行（或 Start 已失败）时，框架会逆序调用 Stop 做资源清理。
// 实现不得假设 Start 必然先于 Stop。Stop 返回的错误会被框架收集，
// 与 OnStop 钩子错误一起由 Run() 统一上抛给调用方。
type Lifecycle interface {
	Init(ctx AppContext) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Service 是可由 App 托管生命周期的命名服务。
type Service interface {
	Name() string
	Lifecycle
}

// Ready 是可选接口：服务在进入可工作状态时关闭返回的 channel
// （例如 Listen 成功之后、Serve 之前）。
// OrderedServices 若检测到该接口，会在启动下一个子服务前等待其关闭；
// 未实现则回退到 Checker 轮询，再否则视为已 invoke 即就绪。
// 仅在成功跨过启动门槛后关闭；Listen/Start 失败不得关闭，以便等待方
// 通过 Start 返回的错误结束，而不会误判为已就绪。
type Ready interface {
	Ready() <-chan struct{}
}

// ServiceFactory 按 FactoryOptions 描述的方式构建服务实例。
type ServiceFactory interface {
	New() Service
	Options() FactoryOptions
}

// FactoryOptions 描述服务构建参数。
type FactoryOptions struct {
	Instances int `json:"instances"` // 实例数
}

func (o *FactoryOptions) ensureDefaults() {
	if o.Instances < 1 {
		o.Instances = 1
	}
}

// HealthCheckersFunc 是健康检查器的取值函数，用于 server 类服务的健康检查
// 配置。方法值 app.HealthCheckers 天然匹配该签名。
type HealthCheckersFunc func() []Checker
