package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"github.com/robfig/cron/v3"
)

// Scheduler 是基于 cron 的定时任务调度组件，实现 lynx.ServerLike 接口。
type Scheduler struct {
	options *Options
	tasks   []Task
	cron    *cron.Cron
	app     lynx.App
	started atomic.Bool
}

// Options 是调度器组件的配置项。
type Options struct {
	Cron         *cron.Cron
	Logger       *slog.Logger
	DebugEnabled bool
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

// Init 记录应用实例。
func (s *Scheduler) Init(app lynx.App) error {
	s.app = app
	return nil
}

// Start 启动 cron 调度器并开始按调度执行任务。
func (s *Scheduler) Start(ctx context.Context) error {
	s.started.Store(true)
	s.cron.Run()
	return nil
}

// Stop 停止 cron 调度器。
func (s *Scheduler) Stop(ctx context.Context) {
	s.cron.Stop()
	s.started.Store(false)
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
		cronInstance = cron.New(cron.WithSeconds(), cron.WithLogger(logger), cron.WithChain(cron.Recover(logger)))
	}

	scheduler := &Scheduler{options: o, cron: cronInstance, tasks: tasks}
	for i := range tasks {
		task := tasks[i]
		if _, err := scheduler.cron.AddFunc(task.Cron(), func() {
			ctx := log.WithContext(context.Background(), "component", "scheduler", "task_name", task.Name())
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "schedule task panic", fmt.Errorf("%v", r))
				}
			}()
			if err := task.HandlerFunc()(ctx); err != nil {
				log.ErrorContext(ctx, "schedule task execute error", err)
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
