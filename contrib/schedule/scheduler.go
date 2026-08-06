// Package schedule 提供基于 robfig/cron 的定时任务调度组件。
package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/robfig/cron/v3"
)

// Scheduler 是基于 cron 的定时任务调度组件，实现 lynx.ServerLike 接口。
type Scheduler struct {
	options *Options
	tasks   []Task
	cron    *cron.Cron
	// logger 是组件日志实例：Init(env) 时从 env.Logger 取，未 Init 时
	// 回落 NewScheduler 的 WithLogger（默认 slog.Default()）。
	logger *slog.Logger
	// taskCtx 是任务执行的上下文：Init(env) 时取自 env.Context()（携带
	// 应用元数据，并在应用关闭时取消）；脱离框架单用（Init(nil)）时任务
	// 使用 Background。
	taskCtx context.Context
	started atomic.Bool
	// stopping 标记 Stop 已被调用：Start 在调度前检查它，避免 Stop 先于
	// Start 时 cron.Stop() 空转（robfig/cron 未 running 时 Stop 不发信号，
	// 随后 Run 进入无人能停的无限循环）。
	stopping atomic.Bool
	// runDone 在 Start 退出（ctx 取消）时关闭，Stop 可借此等待调度循环
	// 收敛；从未启动时由 Stop 幂等关闭。
	runDone  chan struct{}
	doneOnce sync.Once
}

// Options 是调度器组件的配置项。
type Options struct {
	Cron         *cron.Cron
	Logger       *slog.Logger
	DebugEnabled bool
	// Location 是任务调度的时区，nil 时使用 time.Local（cron 默认）。
	Location *time.Location
	// OnTaskError 是任务执行错误回调；nil 时保持默认日志输出。
	OnTaskError func(ctx context.Context, task Task, err error)
}

// CheckHealth 实现健康检查，调度器未初始化或未运行时返回错误。
func (s *Scheduler) CheckHealth() error {
	if s.cron == nil {
		return errors.New("scheduler not initialized")
	}
	if !s.started.Load() {
		return errors.New("scheduler not running")
	}
	return nil
}

// Name 返回组件名称 "cron-scheduler"。
func (s *Scheduler) Name() string {
	return "cron-scheduler"
}

// Init 记录任务上下文与日志实例。env 为 nil（脱离框架单用）时保持
// 默认值：任务上下文回退 Background。
func (s *Scheduler) Init(env lynx.Env) error {
	if env == nil {
		return nil
	}
	s.taskCtx = env.Context()
	s.logger = env.Logger("component", "cron-scheduler")
	return nil
}

// Start 启动 cron 调度器并开始按调度执行任务，阻塞至传入 ctx 取消。
// 竞态安全：Stop 先于本方法调用时（组件启动失败引发的提前中断），
// 不启动 cron 并立即返回，保证 run.Group 不会因停不掉的 cron 循环挂死。
func (s *Scheduler) Start(ctx context.Context) error {
	if s.stopping.Load() {
		// Stop 先到：不启动 cron。runDone 已由 Stop 关闭（或在此关闭），
		// 等待方不会挂起。
		s.closeRunDone()
		return nil
	}
	s.started.Store(true)
	// 同步启动：cron.Start 置位 running 后，任何时刻的 Stop 都能设置
	// stop channel 使内部循环退出。
	s.cron.Start()
	// 复查：Stop 可能刚好在上一行之前执行（其 cron.Stop 因 running 未置位
	// 而空转，且读到的 started 为 false 已提前返回），此处补发停止信号并
	// 直接返回错误——不得进入下方的阻塞等待，否则 ctx 永无人取消，
	// Start 挂死（Stop/Start 交错窗口）。
	if s.stopping.Load() {
		s.cron.Stop()
		s.started.Store(false)
		s.closeRunDone()
		return errors.New("scheduler stopped before start")
	}
	// 对齐 run.Group actor 语义：等待传入的 ctx 取消（框架在 Stop 返回后
	// 取消组件 ctx）。任务执行的取消由 taskCtx（env.Context）在应用关闭时
	// 触发，与 Start 的等待相互独立。
	<-ctx.Done()
	s.started.Store(false)
	s.closeRunDone()
	return nil
}

// closeRunDone 关闭 runDone（幂等）；runDone 未初始化（绕过
// NewScheduler 构造）时静默跳过。
func (s *Scheduler) closeRunDone() {
	if s.runDone == nil {
		return
	}
	s.doneOnce.Do(func() { close(s.runDone) })
}

// Stop 停止 cron 调度器。cron.Stop 发出停止信号，调度循环随即退出；
// 等待 runDone 收敛受调用方 deadline 约束（调用方无 deadline 时立即
// 返回，由框架的 StopTimeout 统一兜底）。
// 停止后调度器不可重启（stopping 永久置位）：需要再次运行时请重新构造
// Scheduler。
func (s *Scheduler) Stop(ctx context.Context) error {
	s.stopping.Store(true)
	// cron 未启动或已停止：无在途调度循环，无需等待。
	if !s.started.Load() {
		s.closeRunDone()
		return nil
	}
	s.cron.Stop()
	if _, ok := ctx.Deadline(); ok {
		select {
		case <-s.runDone:
		case <-ctx.Done():
		}
	}
	return nil
}

var _ lynx.ServerLike = new(Scheduler)

// Task 定义一个定时任务：名称、cron 表达式与处理函数。
type Task interface {
	Name() string
	Cron() string
	HandlerFunc() HandlerFunc
}

// HandlerFunc 是定时任务执行的处理函数。
type HandlerFunc func(ctx context.Context) error

// Option 用于配置调度器 Options 的选项函数。
type Option func(*Options)

// WithLogger 设置调度器的日志实例。
func WithLogger(logger *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = logger
	}
}

// WithCron 设置自定义的 cron 实例；未设置时使用内置默认实例。
func WithCron(cron *cron.Cron) Option {
	return func(o *Options) {
		o.Cron = cron
	}
}

// WithDebugEnabled 开启 cron 调试日志输出。
func WithDebugEnabled() Option {
	return func(o *Options) {
		o.DebugEnabled = true
	}
}

// WithLocation 设置任务调度的时区；未设置时使用 time.Local。
// 仅对内置默认 cron 实例生效：使用 WithCron 传入自定义实例时，
// 请在其构造中自行设置 Location。
func WithLocation(loc *time.Location) Option {
	return func(o *Options) {
		o.Location = loc
	}
}

// WithErrorHandler 设置任务执行错误回调（context 携带任务名与调度器元数据）；
// 未设置时保持默认日志输出。
// 注意：仅接收任务 HandlerFunc 返回的错误；任务 panic 由调度器恢复并记
// 日志，不触发该回调。
func WithErrorHandler(fn func(ctx context.Context, task Task, err error)) Option {
	return func(o *Options) {
		o.OnTaskError = fn
	}
}

// NewScheduler 创建调度器组件并注册所有定时任务，cron 表达式非法时返回错误。
func NewScheduler(tasks []Task, opts ...Option) (*Scheduler, error) {
	o := &Options{
		Logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(o)
	}
	logger := NewSlogLogger(o.Logger, o.DebugEnabled)
	var cronInstance *cron.Cron
	if o.Cron != nil {
		cronInstance = o.Cron
	} else {
		cronOpts := []cron.Option{
			cron.WithSeconds(),
			cron.WithLogger(logger),
			// SkipIfStillRunning 防止任务执行时间超过间隔时重叠运行。
			cron.WithChain(cron.Recover(logger), cron.SkipIfStillRunning(logger)),
		}
		if o.Location != nil {
			cronOpts = append(cronOpts, cron.WithLocation(o.Location))
		}
		cronInstance = cron.New(cronOpts...)
	}

	scheduler := &Scheduler{
		options: o,
		logger:  o.Logger,
		cron:    cronInstance,
		tasks:   tasks,
		runDone: make(chan struct{}),
	}
	for i := range tasks {
		task := tasks[i]
		if _, err := scheduler.cron.AddFunc(task.Cron(), func() {
			// 任务上下文取自 Init（env.Context，携带应用元数据，关闭时
			// 取消）；未 Init 时回退 Background。
			ctx := scheduler.taskCtx
			if ctx == nil {
				ctx = context.Background()
			}
			defer func() {
				if r := recover(); r != nil {
					scheduler.logger.ErrorContext(ctx, "schedule task panic",
						"task_name", task.Name(), "error", fmt.Errorf("%v", r))
				}
			}()
			if err := task.HandlerFunc()(ctx); err != nil {
				if scheduler.options.OnTaskError != nil {
					scheduler.options.OnTaskError(ctx, task, err)
				} else {
					scheduler.logger.ErrorContext(ctx, "schedule task execute error",
						"task_name", task.Name(), "error", err)
				}
			}
		}); err != nil {
			return nil, err
		}
	}

	return scheduler, nil
}

// NewSlogLogger 将 slog 实例适配为 cron.Logger；logDebug 为 true 时输出调试日志。
func NewSlogLogger(slogger *slog.Logger, logDebug bool) cron.Logger {
	return &slogLogger{slogger: slogger, logDebug: logDebug}
}

type slogLogger struct {
	slogger  *slog.Logger
	logDebug bool
}

func (l *slogLogger) Info(msg string, keysAndValues ...interface{}) {
	if l.logDebug {
		l.slogger.Debug(msg, keysAndValues...)
	}
}

func (l *slogLogger) Error(err error, msg string, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "error", err)
	l.slogger.Error(msg, keysAndValues...)
}
