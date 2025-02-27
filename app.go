package lynx

import (
	"context"
	"emperror.dev/emperror"
	"github.com/lynx-go/lynx/hook"
	"github.com/lynx-go/x/log"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type RunFunc func(ctx context.Context) error

func RunWaitSignal() RunFunc {
	return func(ctx context.Context) error {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		return nil
	}
}

type SetupFunc[O any] func(ctx context.Context, hooks *hook.Hooks, o O, args []string) (RunFunc, error)

type App[O any] struct {
	onSetup       SetupFunc[O]
	name          string
	version       string
	registrar     *hook.Hooks
	logger        *slog.Logger
	mux           sync.Mutex
	isInitialized bool
	isClosed      bool
}

func (app *App[O]) Name() string {
	return app.name
}

func (app *App[O]) RunE(ctx context.Context, o O, args []string) error {
	app.mux.Lock()
	defer app.mux.Unlock()

	app.registrar = &hook.Hooks{}
	var cancelCtx context.CancelFunc
	ctx, cancelCtx = context.WithCancel(ctx)
	defer cancelCtx()

	ctx = log.Context(ctx, app.logger)
	runnable, err := app.onSetup(ctx, app.registrar, o, args)
	if err != nil {
		return err
	}
	eg, egCtx := errgroup.WithContext(ctx)
	wg := sync.WaitGroup{}
	for _, hk := range app.registrar.Hooks() {
		eg.Go(func() error {
			<-egCtx.Done()
			stopCtx, cancelCtx := context.WithTimeout(egCtx, 5*time.Second)
			defer cancelCtx()
			return hk.Stop(stopCtx)
		})
		wg.Add(1)
		eg.Go(func() error {
			wg.Done()
			return hk.Start(ctx)
		})
	}
	wg.Wait()

	eg.Go(func() error {
		defer cancelCtx()
		return runnable(ctx)
	})
	app.isInitialized = true

	return eg.Wait()
}

func (app *App[O]) Run(ctx context.Context, o O, args []string) {
	emperror.Panic(app.RunE(ctx, o, args))
}

type Option[O any] func(*App[O])

func WithLogger[O any](logger *slog.Logger) Option[O] {
	return func(a *App[O]) { a.logger = logger }
}

func WithName[O any](name string) Option[O] {
	return func(a *App[O]) {
		a.name = name
	}
}

func WithVersion[O any](version string) Option[O] {
	return func(a *App[O]) {
		a.version = version
	}
}

func WithSetup[O any](setup SetupFunc[O]) Option[O] {
	return func(a *App[O]) {
		a.onSetup = setup
	}
}

func New[O any](opts ...Option[O]) *App[O] {
	app := &App[O]{
		registrar: &hook.Hooks{},
	}
	for _, opt := range opts {
		opt(app)
	}
	if app.logger == nil {
		app.logger = slog.Default().With("name", app.name, "version", app.version)
	}

	return app
}
