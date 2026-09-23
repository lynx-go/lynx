package lynx

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/pflag"
)

// Options 的默认值与校验区间。
const (
	DefaultName            = "lynx-app"
	DefaultShutdownTimeout = 5 * time.Second
	DefaultStopTimeout     = 5 * time.Second
	// DefaultBusReadyTimeout 是 newLynx 等待消息总线就绪的总预算（10 秒）：
	// Watermill+Kafka 等后端的就绪明显慢于默认内存 Bus，1 秒级的硬编码
	// 预算会让正常部署下的构造直接失败，故放宽为可配的 10 秒。
	DefaultBusReadyTimeout = 10 * time.Second
	// DefaultCleanupTimeout 是 OnPostStop 收尾钩子的默认总预算（10 秒）：
	// 对齐典型 Wire cleanup（关闭 DB/Redis 连接池）的合理上界，任何单个
	// Close 挂起都不阻塞进程退出。
	DefaultCleanupTimeout = 10 * time.Second
	// MinTimeout 与 MaxTimeout 是 ShutdownTimeout 与 StopTimeout 共用的
	// 校验区间（1 秒 ~ 5 分钟）。
	MinTimeout = 1 * time.Second
	MaxTimeout = 5 * time.Minute
)

// Options 校验错误。
var (
	// ErrNameTooLong 表示应用名超过 63 字符上限。
	ErrNameTooLong = errors.New("name must be at most 63 characters")
	// ErrShutdownTimeoutTooSmall 表示 ShutdownTimeout 非零但小于 MinTimeout。
	ErrShutdownTimeoutTooSmall = errors.New("shutdown timeout must be at least 1 second")
	// ErrShutdownTimeoutTooLarge 表示 ShutdownTimeout 大于 MaxTimeout。
	ErrShutdownTimeoutTooLarge = errors.New("shutdown timeout must be at most 5 minutes")
	// ErrStopTimeoutTooSmall 表示 StopTimeout 非零但小于 MinTimeout。
	ErrStopTimeoutTooSmall = errors.New("stop timeout must be at least 1 second")
	// ErrStopTimeoutTooLarge 表示 StopTimeout 大于 MaxTimeout。
	ErrStopTimeoutTooLarge = errors.New("stop timeout must be at most 5 minutes")
	// ErrDrainTimeoutInvalid 表示 DrainTimeout 为负值（排水窗口不允许负值）。
	ErrDrainTimeoutInvalid = errors.New("drain timeout must not be negative")
	// ErrBusReadyTimeoutInvalid 表示 BusReadyTimeout 为负值（就绪等待预算不允许负值）。
	ErrBusReadyTimeoutInvalid = errors.New("bus ready timeout must not be negative")
	// ErrCleanupTimeoutInvalid 表示 CleanupTimeout 为负值（收尾预算不允许负值）。
	ErrCleanupTimeoutInvalid = errors.New("cleanup timeout must not be negative")
)

// Options 是 App 应用的核心配置项。
type Options struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	BindFlagsFunc  BindFlagsFunc  `json:"-"`
	BindConfigFunc BindConfigFunc `json:"-"`
	ExitSignals    []os.Signal    `json:"-"`
	Bus            eventbus.Bus   `json:"-"`
	// BusProvider 非 nil 且 WithBus 未注入时，在配置装配完成后由框架调用
	// 以构造应用总线（依赖配置的总线如 watermill.NewFromConfig）及其配套
	// 服务（如 kafka Transport）。与 WithBus 并存时 WithBus 优先。
	BusProvider func(cfg Config) (eventbus.Bus, []Service, error) `json:"-"`
	// ConfigWatch 启用配置文件热更新（WithConfigWatch）：Run 启动期注册
	// viper WatchConfig，文件变更时框架经总线发布 lynx.config.updated
	// 事件（eventbus.ConfigUpdatedTopic），后续 Config() 读取返回新值；
	// 订阅方自行决定响应粒度。要求配置来自文件，否则 Run 启动期报错。
	ConfigWatch bool `json:"-"`
	// busFromOptions 标记 Bus 由框架填充（EnsureDefaults 默认或 WithBusOptions
	// 物化），而非显式 WithBus 注入：provider 咨询条件放宽为"Bus 为 nil 或
	// 仅由框架填充"，使 NewOptions 先跑 EnsureDefaults 的路径与 Option 顺序
	// 都不会静默击败 provider；显式 WithBus 注入时本标记被清除，维持
	// "WithBus 优先于 provider"的既有规则。
	busFromOptions  bool
	ShutdownTimeout time.Duration `json:"shutdown_timeout"`
	// StopTimeout 是单个服务 Stop 的最长等待时长，超过后跳过并记录错误，
	// 防止挂死的服务阻塞整个关停流程。
	// ShutdownTimeout 与 StopTimeout 的 0 均表示"未设置"：EnsureDefaults
	// 会折叠为默认值，无法显式表达"禁用上界"（注意与 server 侧
	// ShutdownTimeout=0 的"无上界"语义不同）。
	StopTimeout time.Duration `json:"stop_timeout"`
	// DrainTimeout 是关停排水（drain）窗口时长，v1.10.0 起同时是
	// OnDrain 钩子的总预算（DrainHookTimeout 已并入）：关停时先让
	// readiness 失败（LB 摘流），OnDrain 钩子（如从服务目录注销）与
	// 窗口睡眠并发执行，窗口结束时无论钩子是否完成都继续后续关停。
	// 0（默认）表示不启用排水——整段（窗口与钩子）禁用；此时注册了
	// OnDrain 钩子会在 Run() 启动期返回 ErrDrainHooksRequireDrainTimeout。
	// 与 ShutdownTimeout 是独立预算：总关停时长上界 =
	// DrainTimeout + ShutdownTimeout + 各服务 StopTimeout 叠加 + CleanupTimeout。
	// 取值任意 ≥0，无下限约束（1ms 等小值合法）。
	DrainTimeout time.Duration `json:"drain_timeout"`
	// BusReadyTimeout 是 newLynx 构造应用时等待消息总线就绪
	// （CheckHealth 通过）的总预算，默认 10 秒：Watermill+Kafka 等
	// 慢启动后端需要比 1 秒级硬编码更宽的就绪窗口。0 表示使用默认值
	//（无法显式禁用等待——总线就绪是构造成功的硬前提）；负值在
	// Validate 时报错。
	BusReadyTimeout time.Duration `json:"bus_ready_timeout"`
	// CleanupTimeout 是 OnPostStop 收尾钩子的总预算（默认 10 秒）：
	// 所有服务与总线停止之后、Run 返回前，逆序执行收尾钩子（关闭
	// DB/Redis 连接池等 DI 底层资源）。与 ShutdownTimeout 是独立预算：
	// 总关停时长上界 = 排水段 + ShutdownTimeout + 各服务 StopTimeout
	// 叠加 + CleanupTimeout。0 表示使用默认值；负值在 Validate 时报错。
	CleanupTimeout time.Duration `json:"cleanup_timeout"`
	// disableConfigFlags 标记用户显式关闭默认 flags（WithDisableConfigFlags）。
	// EnsureDefaults 在 NewOptions 与 newLynx 间可能被多次调用，需要该
	// 标记保持关闭语义不被默认值覆盖。
	disableConfigFlags bool
	// Config 非 nil 时作为应用配置直接采用（WithConfig 注入）：构造期跳过
	// flags 解析与配置文件装配（BindFlagsFunc/BindConfigFunc 不再执行），
	// os.Args 与工作目录不参与配置。用于测试与宿主进程注入。
	Config Config `json:"-"`
	// isolated 标记 WithIsolated：应用不触碰进程级全局（构造时不执行
	// eventbus.SetDefault 与 lynx.Set，SetLogger/日志级别不同步
	// slog.SetDefault）。用于同进程多 App（并行测试、宿主内嵌）场景。
	isolated bool
}

// String 返回 Options 的 JSON 字符串表示（函数类字段不参与序列化）。
func (o *Options) String() string {
	bs, _ := json.Marshal(o)
	return string(bs)
}

// Validate checks if the Options values are valid.
func (o *Options) Validate() error {
	if len(o.Name) > 63 {
		return ErrNameTooLong
	}
	if o.ShutdownTimeout > 0 {
		if o.ShutdownTimeout < MinTimeout {
			return ErrShutdownTimeoutTooSmall
		}
		if o.ShutdownTimeout > MaxTimeout {
			return ErrShutdownTimeoutTooLarge
		}
	}
	if o.StopTimeout > 0 {
		if o.StopTimeout < MinTimeout {
			return ErrStopTimeoutTooSmall
		}
		if o.StopTimeout > MaxTimeout {
			return ErrStopTimeoutTooLarge
		}
	}
	if o.DrainTimeout < 0 {
		return ErrDrainTimeoutInvalid
	}
	if o.BusReadyTimeout < 0 {
		return ErrBusReadyTimeoutInvalid
	}
	if o.CleanupTimeout < 0 {
		return ErrCleanupTimeoutInvalid
	}
	return nil
}

// EnsureDefaults sets default values for unset fields.
// 校验由 Validate 单独负责，newLynx 会在 EnsureDefaults 后调用它。
func (o *Options) EnsureDefaults() {
	if o.ID == "" {
		o.ID, _ = os.Hostname()
	}

	if o.Name == "" {
		o.Name = DefaultName
	}

	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}

	if o.StopTimeout == 0 {
		o.StopTimeout = DefaultStopTimeout
	}

	if o.BusReadyTimeout == 0 {
		o.BusReadyTimeout = DefaultBusReadyTimeout
	}

	if o.CleanupTimeout == 0 {
		o.CleanupTimeout = DefaultCleanupTimeout
	}

	if len(o.ExitSignals) == 0 {
		// SIGKILL 无法被捕获，列入默认列表只会误导调用方。
		o.ExitSignals = []os.Signal{
			syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT,
		}
	}

	// BusProvider 存在时不预填内存默认总线：总线在配置装配完成后由
	// provider 构造（见 newLynx）；provider 与 WithBus 均未设置时才落默认。
	// 经 NewOptions 先跑 EnsureDefaults 的路径，默认总线在此已被填充并带
	// busFromOptions 标记——newLynx 仍会让 provider 覆盖它。
	if o.Bus == nil && o.BusProvider == nil {
		o.Bus = eventbus.NewMemoryBus(eventbus.Options{})
		o.busFromOptions = true
	}

	// 默认启用框架内置的命令行 flags：不传任何 flags 相关 Option 时
	// 也能解析 -c/--log-level 等（修复"静默失效"陷阱）。显式传入自定义
	// 函数时保留自定义实现；WithDisableConfigFlags 显式关闭。
	if !o.disableConfigFlags {
		if o.BindFlagsFunc == nil {
			o.BindFlagsFunc = DefaultBindFlagsFunc
		}
		if o.BindConfigFunc == nil {
			o.BindConfigFunc = DefaultBindConfigFunc
		}
	}
}

// Option 用于配置 Options 的选项函数。
type Option func(*Options)

// WithID 设置应用实例 ID。
func WithID(id string) Option {
	return func(o *Options) {
		o.ID = id
	}
}

// WithName 设置应用名称。
func WithName(name string) Option {
	return func(o *Options) {
		o.Name = name
	}
}

// WithVersion 设置应用版本号。
func WithVersion(v string) Option {
	return func(o *Options) {
		o.Version = v
	}
}

// WithBindFlagsFunc 设置自定义的命令行 flags 绑定函数。
func WithBindFlagsFunc(f BindFlagsFunc) Option {
	return func(o *Options) {
		o.BindFlagsFunc = f
	}
}

// WithDisableConfigFlags 关闭默认的命令行 flags 与配置绑定。
// 默认行为：未显式设置 BindFlagsFunc/BindConfigFunc 时，框架自动启用内置的
// 参数声明与绑定（见 DefaultBindFlagsFunc/DefaultBindConfigFunc）。
func WithDisableConfigFlags() Option {
	return func(o *Options) {
		o.disableConfigFlags = true
		o.BindFlagsFunc = nil
		o.BindConfigFunc = nil
	}
}

// WithBindConfigFunc 设置自定义的配置绑定函数。
func WithBindConfigFunc(f BindConfigFunc) Option {
	return func(o *Options) {
		o.BindConfigFunc = f
	}
}

// WithConfigFile 设置配置文件路径并关闭默认的命令行 flags，用于参数由
// 外部解析的场景（典型如子命令框架已解析 -c/--config）：路径直接绑定为
// 配置文件，path 为空时回退搜索工作目录（与 DefaultBindConfigFunc 的
// 回退一致）。
// 等价于按序应用 WithDisableConfigFlags 与 WithBindConfigFunc——顺序敏感
// （前者会清空 BindConfigFunc），封装为单一选项消除该陷阱。与其他选项
// 同用时遵循 Option 后到者胜的通用语义。
// 测试场景的配置注入（分层叠加、构造期即时读入）见 lynxtest 包的
// WithConfigBaseline 系列选项。
func WithConfigFile(path string) Option {
	return func(o *Options) {
		o.disableConfigFlags = true
		o.BindFlagsFunc = nil
		o.BindConfigFunc = func(_ *pflag.FlagSet, c ConfigSource) error {
			if path != "" {
				c.SetFile(path)
				return nil
			}
			c.AddSearchPath(".")
			return nil
		}
	}
}

// WithExitSignals 设置触发应用退出的操作系统信号。
func WithExitSignals(signals ...os.Signal) Option {
	return func(o *Options) {
		o.ExitSignals = signals
	}
}

// WithShutdownTimeout 设置应用优雅关停的超时时间。
// 注意：0 不是"禁用上界"，会被 EnsureDefaults 折叠为默认值（5 秒），
// 显式禁用上界当前无法表达（与 server 侧 ShutdownTimeout=0 的
// "无上界"语义不一致，历史行为冻结不改）。
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ShutdownTimeout = timeout
	}
}

// WithStopTimeout 设置单个服务 Stop 的最长等待时长，超过后跳过并记录错误。
// 注意：0 不是"禁用上界"，会被 EnsureDefaults 折叠为默认值（5 秒），
// 显式禁用上界当前无法表达（与 server 侧 ShutdownTimeout=0 的
// "无上界"语义不一致，历史行为冻结不改）。
func WithStopTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.StopTimeout = timeout
	}
}

// WithDrainTimeout 设置关停排水（drain）窗口时长：关停信号到达后先让
// readiness 失败（LB 摘流），等待该窗口结束后才真正关停。窗口同时是
// OnDrain 钩子的总预算（钩子与窗口睡眠并发执行，窗口结束即继续关停）。
// 0（默认）表示不启用排水——整段（窗口与钩子）禁用；此时注册了
// OnDrain 钩子会在 Run() 启动期返回 ErrDrainHooksRequireDrainTimeout。
// 与 ShutdownTimeout 是独立预算：总关停时长上界 =
// DrainTimeout + ShutdownTimeout + 各服务 StopTimeout 叠加 + CleanupTimeout。
// 取值任意 ≥0，无下限约束。
func WithDrainTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.DrainTimeout = timeout
	}
}

// WithBusReadyTimeout 设置 newLynx 等待消息总线就绪的总预算（默认 10 秒）。
// Watermill+Kafka 等后端的就绪明显慢于内存 Bus，可按部署环境调宽；
// 0 表示回退默认值（无法显式禁用），负值会在 Validate 时报错。
func WithBusReadyTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.BusReadyTimeout = timeout
	}
}

// WithCleanupTimeout 设置 OnPostStop 收尾钩子的总预算（默认 10 秒）：
// 所有服务与总线停止之后、Run 返回前，逆序执行收尾钩子（如 Wire
// cleanup 关闭 DB/Redis 连接池）。超时记日志并跳过剩余钩子，不阻塞
// 进程退出。与 ShutdownTimeout 是独立预算。0 表示回退默认值，
// 负值会在 Validate 时报错。
func WithCleanupTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.CleanupTimeout = timeout
	}
}

// WithBus 注入自定义消息总线；nil 时框架使用内存默认总线（开箱即用）。
func WithBus(b eventbus.Bus) Option {
	return func(o *Options) {
		o.Bus = b
		o.busFromOptions = false
	}
}

// WithBusProvider 设置配置驱动的总线构造器：框架在构造序列内、配置装配
// 完成后以装配好的 Config 调用，用于依赖配置的总线（如
// watermill.NewFromConfig——此前这类总线必须在 NewRunner 之前自行读取
// 配置再经 WithBus 注入）。返回的服务（如 kafka Transport）按 Register
// 语义托管：Init 同步执行、Start/Stop 纳入生命周期、实现 Checker 的进入
// 健康聚合（CLI 命令的健康等待因此能等 Transport 就绪）。
// 与 WithBus 并存时 WithBus 优先（provider 仅在 Bus 为 nil 时被咨询）；
// provider 返回 nil 总线视为构造错误。
func WithBusProvider(fn func(cfg Config) (eventbus.Bus, []Service, error)) Option {
	return func(o *Options) {
		o.BusProvider = fn
	}
}

// WithConfigWatch 启用配置文件热更新（viper WatchConfig）：文件变更时
// 框架发布 lynx.config.updated 事件（eventbus.ConfigUpdatedTopic，
// 订阅示例见该 Topic 注释），此后 Config() 读取返回新值——订阅方收到
// 事件后重新读取配置并自行决定响应粒度（重建连接/调参/忽略）。
// 仅对文件来源的配置生效（--config / -c 或 WithConfigFile）：无配置
// 文件时 Run() 启动期返回错误（显式要求热更新却无从 watch，快失败
// 好过静默失效）。WithConfig 注入自定义 Config 实现的场景同样报错
// （无 viper 文件句柄可 watch）。
func WithConfigWatch() Option {
	return func(o *Options) {
		o.ConfigWatch = true
	}
}

// WithConfig 注入现成的应用配置：构造期跳过 flags 解析、配置文件搜索与
// BindPFlags（BindFlagsFunc/BindConfigFunc 不再执行），os.Args 与工作目录
// 不再参与装配。init 仍从该配置读取 service.name/id/version 元数据并应用
// logging.level 日志级别。nil 传入无效（保持默认装配路径）。
// 典型用于测试（配合 lynxtest）与宿主进程内嵌场景。
func WithConfig(c Config) Option {
	return func(o *Options) {
		if c != nil {
			o.Config = c
		}
	}
}

// WithIsolated 使应用不触碰进程级全局状态：构造时不执行 eventbus.SetDefault
// 与 lynx.Set，SetLogger 与日志级别应用不再同步 slog.SetDefault。用于同一
// 进程内构造多个 App（表驱动/并行测试、宿主内嵌）互不污染全局。
// 代价：依赖全局取值的代码（eventbus.Default()、lynx.Get()、裸 slog 调用）
// 看不到该实例，需要改用注入的 AppContext/Bus。默认关闭。
func WithIsolated() Option {
	return func(o *Options) {
		o.isolated = true
	}
}

// WithBusOptions 以选项配置默认内存总线（Bus 为 nil 时生效；已注入 Bus 时无视）。
// BusProvider 已设置时同样无视：配置驱动的总线由 provider 构造，内存总线
// 选项无意义。若本选项先于 WithBusProvider 应用（此时物化了内存总线），
// provider 仍会覆盖物化结果——两个选项对顺序不敏感，provider 不会被静默击败。
func WithBusOptions(opts ...eventbus.Option) Option {
	return func(o *Options) {
		if o.Bus != nil || o.BusProvider != nil {
			return
		}
		bo := eventbus.Options{}
		for _, fn := range opts {
			fn(&bo)
		}
		o.Bus = eventbus.NewMemoryBus(bo)
		o.busFromOptions = true
	}
}

// NewOptions 创建带默认值的 Options，并按顺序应用给定的选项。
func NewOptions(opts ...Option) *Options {
	o := &Options{}
	o.EnsureDefaults()
	for _, opt := range opts {
		opt(o)
	}
	return o
}
