package lynx

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"time"
)

// Options 的默认值与校验区间。
const (
	DefaultName            = "lynx-app"
	DefaultShutdownTimeout = 5 * time.Second
	DefaultStopTimeout     = 5 * time.Second
	// DefaultDrainHookTimeout 是 OnDrain 钩子的默认总预算（3 秒）。
	DefaultDrainHookTimeout = 3 * time.Second
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
	// ErrDrainHookTimeoutInvalid 表示 DrainHookTimeout 为负值。
	ErrDrainHookTimeoutInvalid = errors.New("drain hook timeout must not be negative")
)

// Options 是 App 应用的核心配置项。
type Options struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	SetFlagsFunc    SetFlagsFunc   `json:"-"`
	BindConfigFunc  BindConfigFunc `json:"-"`
	ExitSignals     []os.Signal    `json:"-"`
	ShutdownTimeout time.Duration  `json:"shutdown_timeout"`
	// StopTimeout 是单个服务 Stop 的最长等待时长，超过后跳过并记录错误，
	// 防止挂死的服务阻塞整个关停流程。
	StopTimeout time.Duration `json:"stop_timeout"`
	// DrainTimeout 是关停排水（drain）窗口时长：0 表示不启用排水（默认，
	// 向后兼容，关停行为与 v1.0 完全一致）。启用后，关停流程先让
	// readiness 失败（LB 摘流），等待该窗口结束后才执行后续关停。
	// DrainTimeout 与 ShutdownTimeout 是两段独立预算，总关停时长上界 =
	// DrainTimeout + ShutdownTimeout + 各服务 StopTimeout 叠加的既有上界。
	// 取值任意 ≥0，无下限约束（1ms 等小值合法）。
	DrainTimeout time.Duration `json:"drain_timeout"`
	// DrainHookTimeout 是 OnDrain 钩子的总预算（默认 3 秒）。OnDrain 钩子在
	// 排水置位后与 DrainTimeout 睡眠并发执行（如从服务目录注销）：有钩子
	// 时关停时长上界 = max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout
	// + 各服务 StopTimeout；无钩子（默认）时该项不计入，行为与之前一致。
	DrainHookTimeout time.Duration `json:"drain_hook_timeout"`
	// disableConfigFlags 标记用户显式关闭默认 flags（WithDisableConfigFlags）。
	// EnsureDefaults 在 NewOptions 与 newLynx 间可能被多次调用，需要该
	// 标记保持关闭语义不被默认值覆盖。
	disableConfigFlags bool
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
	if o.DrainHookTimeout < 0 {
		return ErrDrainHookTimeoutInvalid
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

	if o.DrainHookTimeout == 0 {
		o.DrainHookTimeout = DefaultDrainHookTimeout
	}

	if len(o.ExitSignals) == 0 {
		// SIGKILL 无法被捕获，列入默认列表只会误导调用方。
		o.ExitSignals = []os.Signal{
			syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT,
		}
	}

	// 默认启用框架内置的命令行 flags：不传任何 flags 相关 Option 时
	// 也能解析 -c/--log-level 等（修复"静默失效"陷阱）。显式传入自定义
	// 函数时保留自定义实现；WithDisableConfigFlags 显式关闭。
	if !o.disableConfigFlags {
		if o.SetFlagsFunc == nil {
			o.SetFlagsFunc = DefaultSetFlagsFunc
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

// WithSetFlagsFunc 设置自定义的命令行 flags 注册函数。
func WithSetFlagsFunc(f SetFlagsFunc) Option {
	return func(o *Options) {
		o.SetFlagsFunc = f
	}
}

// WithDisableConfigFlags 关闭默认的命令行 flags 与配置绑定。
// 默认行为：未显式设置 SetFlagsFunc/BindConfigFunc 时，框架自动启用内置的
// 参数声明与绑定（见 DefaultSetFlagsFunc/DefaultBindConfigFunc）。
func WithDisableConfigFlags() Option {
	return func(o *Options) {
		o.disableConfigFlags = true
		o.SetFlagsFunc = nil
		o.BindConfigFunc = nil
	}
}

// WithBindConfigFunc 设置自定义的配置绑定函数。
func WithBindConfigFunc(f BindConfigFunc) Option {
	return func(o *Options) {
		o.BindConfigFunc = f
	}
}

// WithExitSignals 设置触发应用退出的操作系统信号。
func WithExitSignals(signals ...os.Signal) Option {
	return func(o *Options) {
		o.ExitSignals = signals
	}
}

// WithShutdownTimeout 设置应用优雅关停的超时时间。
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ShutdownTimeout = timeout
	}
}

// WithStopTimeout 设置单个服务 Stop 的最长等待时长，超过后跳过并记录错误。
func WithStopTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.StopTimeout = timeout
	}
}

// WithDrainTimeout 设置关停排水（drain）窗口时长：关停信号到达后先让
// readiness 失败（LB 摘流），等待该窗口结束后才真正关停。0（默认）表示
// 不启用排水，关停行为与 v1.0 完全一致。DrainTimeout 与 ShutdownTimeout
// 是两段独立预算：总关停时长上界 = DrainTimeout + ShutdownTimeout + 各服务
// StopTimeout 叠加的既有上界。取值任意 ≥0，无下限约束。
func WithDrainTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.DrainTimeout = timeout
	}
}

// WithDrainHookTimeout 设置 OnDrain 钩子的总预算（默认 3 秒）。钩子在
// 排水置位后与 DrainTimeout 睡眠并发执行；注册 OnDrain 钩子时，关停
// 时长上界 = max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout +
// 各服务 StopTimeout。负值会在 Validate 时报错。
func WithDrainHookTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.DrainHookTimeout = timeout
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
