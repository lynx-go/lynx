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

// healthCheckTimeout 是单次 CheckHealth 的上界。Checker 接口（无 ctx
// 参数）已冻结，挂死的 checker 若不加防护会让单次尝试永久阻塞，
// MaxTries/MaxBackoff 的重试上限全部失效；超时按"未就绪"参与重试。
const healthCheckTimeout = 3 * time.Second

// readyWaitTimeout 是单次 Ready channel 等待的上界，与 healthCheckTimeout
// 同值同理：超时按"本轮未就绪"参与退避重试。Ready 是单调边沿信号，
// 已闭合的 channel 在后续轮次经非阻塞检查立即通过，无重复等待成本。
const readyWaitTimeout = healthCheckTimeout

// checkHealthBounded 以固定上界执行单次健康检查：goroutine+select 兜底
// 无 ctx 的 checker。结果经缓冲 chan 返回，超时后迟到的结果被自然丢弃
// （goroutine 不因无人接收而阻塞；若 checker 永久挂死，该 goroutine
// 随之遗留——保证等待循环不挂死优先，与 stopServiceBounded 同一取舍）。
// 不监听调用侧 ctx：健康的 checker 立即返回即成功（即使 ctx 已取消，
// 既有语义为"首查健康即运行"）；取消裁决由 backoff.Retry 在重试间完成。
func checkHealthBounded(checker Checker) error {
	done := make(chan error, 1)
	go func() { done <- checker.CheckHealth() }()
	select {
	case err := <-done:
		return err
	case <-time.After(healthCheckTimeout):
		return fmt.Errorf("health check timed out after %v", healthCheckTimeout)
	}
}

// waitReadyBounded 以固定上界单次等待 Ready channel：超过上界视为"本轮
// 未就绪"参与退避重试（与 checkHealthBounded 对称的取舍）。已闭合的
// channel 经先行的非阻塞检查立即成功——即使 ctx 已取消也放行（保持
// "首查即就绪即运行"的既有语义）；尚未闭合时监听 ctx：组中断（如依赖
// Start 失败触发 run group 中断）立即退出，失败裁决归出错方经 Run
// 上抛。Ready 契约保证 channel 只在成功跨过启动门槛后关闭，不会因
// 失败而误判就绪。
func waitReadyBounded(ctx context.Context, r Ready) error {
	ready := r.Ready()
	select {
	case <-ready:
		return nil
	default:
	}
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(readyWaitTimeout):
		return fmt.Errorf("ready signal not closed within %v", readyWaitTimeout)
	}
}

// depProbe 是命令等待的单个依赖探测项：name 供日志定位，probe 单次
// 探测（nil = 就绪，err = 未就绪或中止）。
type depProbe struct {
	name  string
	probe func(ctx context.Context) error
}

// dependencyProbes 按三级优先解析依赖探测项（与 OrderedServices 的启动
// 顺序解析同款）：实现 Ready 的服务等待 channel（边沿信号：单调、失败
// 不关闭，失败裁决归 Start 返回值）；否则实现 Checker 的有界轮询；
// 两者皆无视为 invoke 即就绪，不等待。命令本身两者皆不实现，天然落在
// 第三级，无需自排除。appctx 为外部 AppContext 实现时回退健康检查
// 聚合（既有行为）。
func (cmd *command) dependencyProbes() []depProbe {
	var probes []depProbe
	if l, ok := cmd.appctx.(*lynx); ok {
		for _, s := range l.serviceSnapshot() {
			switch v := s.(type) {
			case Ready:
				probes = append(probes, depProbe{
					name: s.Name(),
					probe: func(ctx context.Context) error {
						return waitReadyBounded(ctx, v)
					},
				})
			case Checker:
				checker := v
				probes = append(probes, depProbe{
					name: s.Name(),
					probe: func(context.Context) error {
						return checkHealthBounded(checker)
					},
				})
			}
		}
		return probes
	}
	for _, checker := range cmd.appctx.HealthCheckers() {
		c := checker
		probes = append(probes, depProbe{
			name:  "checker",
			probe: func(context.Context) error { return checkHealthBounded(c) },
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
