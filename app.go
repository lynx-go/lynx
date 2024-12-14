package lynx

import (
	"context"
	"github.com/lynx-go/lynx/log"
	"github.com/oklog/run"
	"log/slog"
	"time"
)

type MD struct {
}

type Hook func(ctx context.Context) error

type Option func(*App)

func WithServices(services ...Service) Option {
	return func(app *App) {
		app.services = append(app.services, services...)
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(app *App) {
		app.logger = logger
	}
}

func WithOnStartHooks(hooks ...Hook) Option {
	return func(app *App) {
		app.onStartHooks = append(app.onStartHooks, hooks...)
	}
}

func WithOnStopHooks(hooks ...Hook) Option {
	return func(app *App) {
		app.onStopHooks = append(app.onStopHooks, hooks...)
	}
}

func New(opts ...Option) *App {
	app := newApp()
	app.ctx, app.cancelCtx = context.WithCancel(context.Background())
	for _, opt := range opts {
		opt(app)
	}

	return app
}

func newApp() *App {
	return &App{
		logger:          slog.Default(),
		shutdownTimeout: 10 * time.Second,
	}
}

type App struct {
	services        []Service
	onStartHooks    []Hook
	onStopHooks     []Hook
	shutdownTimeout time.Duration
	logger          *slog.Logger
	ctx             context.Context
	cancelCtx       context.CancelFunc
}

func (app *App) shutdownCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), app.shutdownTimeout)
}

func (app *App) serviceHandler(svc Service) (execute func() error, interrupt func(error)) {
	ctx, cancel := context.WithCancel(app.ctx)
	return func() error {
			ctx := log.Context(ctx, app.logger)
			return svc.Start(ctx)
		}, func(err error) {
			app.logger.Warn("shutdown service", "cause", err)
			defer cancel()
			ctx, cancelCtx := app.shutdownCtx()
			defer cancelCtx()
			if err := svc.Stop(ctx); err != nil {
				app.logger.Error("failed to shutdown service", "error", err)
			}
		}
}

func (app *App) runHook(h Hook) error {
	ctx := log.Context(app.ctx, app.logger)
	return h(ctx)
}

func (app *App) hookHandler() (execute func() error, interrupt func(error)) {
	ctx, cancel := context.WithCancel(app.ctx)
	return func() error {
			for _, h := range app.onStartHooks {
				if err := app.runHook(h); err != nil {
					return err
				}
			}
			select {
			case <-ctx.Done():
				return nil
			}
		}, func(err error) {
			sctx, cancelCtx := app.shutdownCtx()
			defer cancelCtx()
			for _, h := range app.onStopHooks {
				_ = h(log.Context(sctx, app.logger))
			}
			cancel()
		}
}

func (app *App) Run() (err error) {
	var group run.Group
	for _, svc := range app.services {
		svc := svc
		group.Add(app.serviceHandler(svc))
	}

	group.Add(app.hookHandler())
	err = group.Run()
	defer app.cancelCtx()
	return err
}
