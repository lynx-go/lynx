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
	MinShutdownTimeout     = 1 * time.Second
	MaxShutdownTimeout     = 5 * time.Minute
)

// Validation errors for Options.
var (
	ErrNameTooLong          = errors.New("name must be at most 63 characters")
	ErrCloseTimeoutTooSmall = errors.New("close timeout must be at least 1 second")
	ErrCloseTimeoutTooLarge = errors.New("close timeout must be at most 5 minutes")
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
	OTel            *OTelOptions   `json:"-"` // 启用框架托管 OTel 初始化（WithOTel）
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
			return ErrCloseTimeoutTooSmall
		}
		if o.ShutdownTimeout > MaxShutdownTimeout {
			return ErrCloseTimeoutTooLarge
		}
	}
	return nil
}

// EnsureDefaults sets default values for unset fields and validates the options.
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

	if len(o.ExitSignals) == 0 {
		o.ExitSignals = []os.Signal{
			syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL,
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

// WithUseDefaultConfigFlagsFunc 使用默认的命令行 flags 注册函数与配置绑定函数。
func WithUseDefaultConfigFlagsFunc() Option {
	return func(o *Options) {
		o.BindConfigFunc = DefaultBindConfigFunc
		o.SetFlagsFunc = DefaultSetFlagsFunc
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

// NewOptions 创建带默认值的 Options，并按顺序应用给定的选项。
func NewOptions(opts ...Option) *Options {
	id, _ := os.Hostname()
	op := &Options{
		ID:              id,
		ExitSignals:     []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL},
		ShutdownTimeout: DefaultShutdownTimeout,
	}
	for _, o := range opts {
		o(op)
	}
	return op
}
