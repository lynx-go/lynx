package schedule

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	options *Options
	tasks   []Task
	cron    *cron.Cron
	app     lynx.Lynx
}

type Options struct {
	Cron         *cron.Cron
	Logger       *slog.Logger
	DebugEnabled bool
}

func (s *Scheduler) CheckHealth() error {
	return nil
}

func (s *Scheduler) Name() string {
	return "cron-scheduler"
}

func (s *Scheduler) Init(app lynx.Lynx) error {
	s.app = app
	for _, t := range s.tasks {
		if _, err := s.cron.AddFunc(t.Cron(), func() {
			t.HandlerFunc()
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) Start(ctx context.Context) error {
	s.cron.Run()
	return nil
}

func (s *Scheduler) Stop(ctx context.Context) {
	s.cron.Stop()
}

var _ lynx.ServerLike = new(Scheduler)

type Task interface {
	Name() string
	Cron() string
	HandlerFunc() HandlerFunc
}

type HandlerFunc func(ctx context.Context) error

type Option func(*Options)

func WithLogger(logger *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = logger
	}
}

func WithCron(cron *cron.Cron) Option {
	return func(o *Options) {
		o.Cron = cron
	}
}

func WithDebugEnabled() Option {
	return func(o *Options) {
		o.DebugEnabled = true
	}
}

func NewScheduler(tasks []Task, opts ...Option) (*Scheduler, error) {
	o := &Options{
		Logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(o)
	}
	logger := newSlogLogger(o.Logger, o.DebugEnabled)
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
			if err := task.HandlerFunc()(ctx); err != nil {
				log.ErrorContext(ctx, "schedule task execute error", err)
			}
		}); err != nil {
			return nil, err
		}
	}

	return scheduler, nil
}

func newSlogLogger(slogger *slog.Logger, debugEnabled bool) cron.Logger {
	return &slogLogger{slogger: slogger, debugEnabled: debugEnabled}
}

type slogLogger struct {
	slogger      *slog.Logger
	debugEnabled bool
}

func (l *slogLogger) Info(msg string, keysAndValues ...interface{}) {
	if l.debugEnabled {
		l.slogger.Debug(msg, keysAndValues...)
	}
}

func (l *slogLogger) Error(err error, msg string, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "error", err)
	l.slogger.Error(msg, keysAndValues...)
}
