package lynx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// CommandFunc 是命令服务执行的业务函数，返回错误时视为命令失败。
type CommandFunc func(ctx context.Context) error

// CommandOptions configures the command service behavior.
type CommandOptions struct {
	Name           string
	MaxTries       uint
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// ProbeTimeout bounds a single dependency probe (one CheckHealth
	// call or one Ready-channel wait). A probe that exceeds it counts
	// as "not ready this round" and joins the backoff retry loop.
	ProbeTimeout time.Duration
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

// WithProbeTimeout sets the upper bound for a single dependency probe
// (health check or ready-signal wait). Non-positive values fall back
// to the 3s default.
func WithProbeTimeout(d time.Duration) CommandOption {
	return func(o *CommandOptions) { o.ProbeTimeout = d }
}

// NewCommand creates a new command service with the given function and options.
func NewCommand(fn CommandFunc, opts ...CommandOption) Service {
	options := &CommandOptions{
		Name:           "command",
		MaxTries:       10,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		ProbeTimeout:   defaultProbeTimeout,
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
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = defaultProbeTimeout
	}
	return &command{fn: fn, options: options}
}

type command struct {
	fn      CommandFunc
	appctx  AppContext
	logger  *slog.Logger
	options *CommandOptions
}

// depProbe 是命令等待的单个依赖探测项：name 供日志定位，probe 单次
// 探测（nil = 就绪，err = 未就绪或中止）。
type depProbe struct {
	name  string
	probe func(ctx context.Context) error
}

// dependencyProbes 按三级优先解析依赖探测项（三级解析的唯一归属在
// ready.go 的 probeServiceReady）：实现 Ready 的服务等待 channel 关闭
// （边沿信号：单调、失败不关闭，失败裁决归 Start 返回值）；否则实现
// Checker 的单次有界健康检查；两者皆无视为 invoke 即就绪，不等待。
// 命令本身两者皆不实现，天然落在第三级，无需自排除。appctx 为外部
// AppContext 实现时回退健康检查聚合（既有行为）。
func (cmd *command) dependencyProbes() []depProbe {
	var probes []depProbe
	if l, ok := cmd.appctx.(*lynx); ok {
		for _, s := range l.serviceSnapshot() {
			// Checker 层显式传 context.Background()：探测结果优先于调用侧
			// 取消（"首查健康即成功"），取消裁决由 backoff.Retry 在重试间
			// 完成（既有语义，ready.go 的 checkHealthBounded 文档）。
			if probe := probeServiceReady(s, cmd.options.ProbeTimeout, context.Background()); probe != nil {
				probes = append(probes, depProbe{name: s.Name(), probe: probe})
			}
		}
		return probes
	}
	for _, checker := range cmd.appctx.HealthCheckers() {
		c := checker
		probes = append(probes, depProbe{
			name: "checker",
			probe: func(context.Context) error {
				return checkHealthBounded(context.Background(), c, cmd.options.ProbeTimeout)
			},
		})
	}
	return probes
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

// Start 等待全部依赖就绪后执行命令。就绪解析三级优先（见
// dependencyProbes）；MaxTries/Backoff 保持轮次预算语义不变。
func (cmd *command) Start(ctx context.Context) error {
	if cmd.appctx == nil {
		return ErrNotInitialized
	}
	if err := cmd.waitForDependencies(ctx); err != nil {
		return err
	}
	return cmd.fn(ctx)
}

// waitForDependencies 以退避重试逐轮探测全部依赖，全部就绪后返回。
// 探测项在入口一次构建：注册先于 Run 的契约保证集合不变，且 Ready/
// CheckHealth 均为幂等探测，逐轮重跑无累计成本。
func (cmd *command) waitForDependencies(ctx context.Context) error {
	probes := cmd.dependencyProbes()
	if len(probes) == 0 {
		return nil
	}
	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.InitialInterval = cmd.options.InitialBackoff
	expBackoff.MaxInterval = cmd.options.MaxBackoff
	if _, err := backoff.Retry(ctx, func() (any, error) {
		for _, p := range probes {
			if err := p.probe(ctx); err != nil {
				cmd.logger.WarnContext(ctx, "waiting for dependent service ready",
					"service", p.name, "error", err)
				return nil, err
			}
		}
		return nil, nil
	}, backoff.WithMaxTries(cmd.options.MaxTries), backoff.WithBackOff(expBackoff)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("aborted waiting for dependencies: %w", ctx.Err())
		}
		return fmt.Errorf("timed out waiting for dependencies to become ready: %w", err)
	}
	return nil
}

func (cmd *command) Stop(ctx context.Context) error {
	if cmd.appctx != nil {
		cmd.appctx.Close()
	}
	return nil
}
