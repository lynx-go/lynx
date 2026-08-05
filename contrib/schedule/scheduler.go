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
	"github.com/lynx-go/x/log"
	"github.com/robfig/cron/v3"
)

// Scheduler 是基于 cron 的定时任务调度组件，实现 lynx.ServerLike 接口。
type Scheduler struct {
	options *Options
	tasks   []Task
	cron    *cron.Cron
	started atomic.Bool
	// stopping 标记 Stop 已被调用：Start 在调度前检查它，避免 Stop 先于
	// Start 时 cron.Stop() 空转（robfig/cron 未 running 时 Stop 不发信号，
	// 随后 Run 进入无人能停的无限循环）。
	stopping atomic.Bool
	// runDone 在 cron 循环退出（或从未启动）时关闭，Stop 等待它解除，
	// 不依赖 cron.Stop() 返回值（未 running 时其为 nil ctx，等待会挂死）。
	runDone chan struct{}
	doneOnce sync.Once
	// mu 保护 ctx/cancel：Init/Start 兜底写入与 Stop 读取可能并发。
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// ensureCtx 确保任务上下文已创建（Init 或 Start 兜底），幂等且并发安全。
func (s *Scheduler) ensureCtx(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancel(context.WithoutCancel(parent))
}

// ensureBackgroundCtx 在无 app 上下文时（Init(nil) 或未经 Init 直接 Start
// 的误用路径）以 Background 创建任务上下文，幂等且并发安全。
func (s *Scheduler) ensureBackgroundCtx() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
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

// Init 创建任务上下文（携带 app 元数据，关闭时取消）。
func (s *Scheduler) Init(app lynx.App) error {
	if app == nil {
		s.ensureBackgroundCtx()
		return nil
	}
	s.ensureCtx(app.Context())
	return nil
}

// Start 启动 cron 调度器并开始按调度执行任务，阻塞至调度器停止。
// 竞态安全：Stop 先于本方法调用时（组件启动失败引发的提前中断），
// 不启动 cron 并立即返回，保证 run.Group 不会因停不掉的 cron 循环挂死。
func (s *Scheduler) Start(ctx context.Context) error {
	if s.stopping.Load() {
		// Stop 先到：不启动 cron。runDone 已由 Stop 关闭（或在此关闭），
		// 等待方不会挂起。
		s.closeRunDone()
		return nil
	}
	// 兜底：直接调用 Start 而未先 Init 时（单测/误用），自建上下文，
	// 避免 nil ctx 调用 panic。
	s.ensureBackgroundCtx()
	s.started.Store(true)
	// 同步启动：cron.Start 置位 running 后，任何时刻的 Stop 都能设置
	// stop channel 使内部循环退出。
	s.cron.Start()
	// 复查：Stop 可能刚好在上一行之前执行（其 cron.Stop 因 running 未置位
	// 而空转），此处补发停止信号自清。
	if s.stopping.Load() {
		s.cron.Stop()
	}
	<-s.ctx.Done()
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

// Stop 停止 cron 调度器并等待 cron 循环退出（受调用方截止时间约束）。
// 任务上下文会被取消，让任务及时感知关闭。
func (s *Scheduler) Stop(ctx context.Context) {
	s.stopping.Store(true)
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// cron 未启动或已停止：无在途调度循环，无需等待。
	if !s.started.Load() {
		s.closeRunDone()
		return
	}
	s.cron.Stop()
	select {
	case <-s.runDone:
	case <-ctx.Done():
	}
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
func WithLocation(loc *time.Location) Option {
	return func(o *Options) {
		o.Location = loc
	}
}

// WithErrorHandler 设置任务执行错误回调（context 携带任务名与调度器元数据）；
// 未设置时保持默认日志输出。
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
		cron:    cronInstance,
		tasks:   tasks,
		runDone: make(chan struct{}),
	}
	for i := range tasks {
		task := tasks[i]
		if _, err := scheduler.cron.AddFunc(task.Cron(), func() {
			// 任务上下文取自调度器（Init 创建），携带 app 元数据并在关闭时取消。
			taskCtx := scheduler.ctx
			if taskCtx == nil {
				taskCtx = context.Background()
			}
			ctx := log.WithContext(taskCtx, "component", "scheduler", "task_name", task.Name())
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "schedule task panic", fmt.Errorf("%v", r))
				}
			}()
			if err := task.HandlerFunc()(ctx); err != nil {
				if scheduler.options.OnTaskError != nil {
					scheduler.options.OnTaskError(ctx, task, err)
				} else {
					log.ErrorContext(ctx, "schedule task execute error", err)
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
