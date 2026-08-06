package lynx

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"time"
)

// Default values for Options.
const (
	DefaultName            = "lynx-app"
	DefaultShutdownTimeout = 5 * time.Second
	DefaultStopTimeout     = 5 * time.Second
	MinShutdownTimeout     = 1 * time.Second
	MaxShutdownTimeout     = 5 * time.Minute
)

// Validation errors for Options.
var (
	ErrNameTooLong            = errors.New("name must be at most 63 characters")
	ErrShutdownTimeoutTooSmall = errors.New("shutdown timeout must be at least 1 second")
	ErrShutdownTimeoutTooLarge = errors.New("shutdown timeout must be at most 5 minutes")
	ErrStopTimeoutTooSmall     = errors.New("stop timeout must be at least 1 second")
	ErrStopTimeoutTooLarge     = errors.New("stop timeout must be at most 5 minutes")
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
	// StopTimeout 是单个组件 Stop 的最长等待时长，超过后跳过并记录错误，
	// 防止挂死的组件阻塞整个关停流程。
	StopTimeout time.Duration `json:"stop_timeout"`
	// disableConfigFlags 标记用户显式关闭默认 flags（WithDisableConfigFlags）。
	// EnsureDefaults 在 NewOptions 与 newLynx 间可能被多次调用，需要该
	// 标记保持关闭语义不被默认值覆盖。
	disableConfigFlags bool
}

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
		if o.ShutdownTimeout < MinShutdownTimeout {
			return ErrShutdownTimeoutTooSmall
		}
		if o.ShutdownTimeout > MaxShutdownTimeout {
			return ErrShutdownTimeoutTooLarge
		}
	}
	if o.StopTimeout > 0 {
		if o.StopTimeout < MinShutdownTimeout {
			return ErrStopTimeoutTooSmall
		}
		if o.StopTimeout > MaxShutdownTimeout {
			return ErrStopTimeoutTooLarge
		}
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

// WithStopTimeout 设置单个组件 Stop 的最长等待时长，超过后跳过并记录错误。
func WithStopTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.StopTimeout = timeout
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
