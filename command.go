package lynx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// CommandFunc 是命令组件执行的业务函数，返回错误时视为命令失败。
type CommandFunc func(ctx context.Context) error

// CommandOptions configures the command service behavior.
type CommandOptions struct {
	Name           string
	MaxTries       uint
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// CommandOption is a function that configures CommandOptions.
type CommandOption func(*CommandOptions)

// WithCommandName sets the command service name used in logs.
func WithCommandName(name string) CommandOption {
	return func(o *CommandOptions) { o.Name = name }
}

// WithMaxTries sets the maximum number of retry attempts.
func WithMaxTries(n uint) CommandOption {
	return func(o *CommandOptions) { o.MaxTries = n }
}

// WithBackoff sets the initial and maximum backoff durations.
func WithBackoff(initial, max time.Duration) CommandOption {
	return func(o *CommandOptions) {
		o.InitialBackoff = initial
		o.MaxBackoff = max
	}
}

// NewCommand creates a new command service with the given function and options.
func NewCommand(fn CommandFunc, opts ...CommandOption) Service {
	options := &CommandOptions{
		Name:           "command",
		MaxTries:       10,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
	}
	for _, opt := range opts {
		opt(options)
	}
	// 钳制非法值，避免 0 次尝试（backoff 视为无限重试）导致启动永久挂起。
	if options.MaxTries == 0 {
		options.MaxTries = 1
	}
	if options.InitialBackoff <= 0 {
		options.InitialBackoff = time.Millisecond
	}
	if options.MaxBackoff < options.InitialBackoff {
		options.MaxBackoff = options.InitialBackoff
	}
	return &command{fn: fn, options: options}
}

type command struct {
	fn      CommandFunc
	appctx  AppContext
	logger  *slog.Logger
	options *CommandOptions
}

func (cmd *command) Name() string {
	return cmd.options.Name
}

func (cmd *command) Init(ctx AppContext) error {
	cmd.appctx = ctx
	if ctx != nil {
		cmd.logger = ctx.Logger("service", cmd.options.Name)
	}
	return nil
}

func (cmd *command) Start(ctx context.Context) error {
	if cmd.appctx == nil {
		return ErrNotInitialized
	}
	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.InitialInterval = cmd.options.InitialBackoff
	expBackoff.MaxInterval = cmd.options.MaxBackoff
	if _, err := backoff.Retry(ctx, func() (any, error) {
		// 每轮重试重新获取健康检查快照：注册先于 Run 的组件在启动过程中
		// 陆续变健康，快照按轮刷新可纳入等待范围（组件必须全部注册在
		// Run 之前，见 App 接口注释）。
		for _, checker := range cmd.appctx.HealthCheckers() {
			if err := checker.CheckHealth(); err != nil {
				cmd.logger.WarnContext(ctx, "waiting for dependent service ready", "error", err)
				return nil, err
			}
		}
		return nil, nil
	}, backoff.WithMaxTries(cmd.options.MaxTries), backoff.WithBackOff(expBackoff)); err != nil {
		return fmt.Errorf("timed out waiting for dependencies to be healthy: %w", err)
	}
	return cmd.fn(ctx)
}

func (cmd *command) Stop(ctx context.Context) error {
	if cmd.appctx != nil {
		cmd.appctx.Close()
	}
	return nil
}
