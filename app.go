package lynx

import (
	"context"
	"emperror.dev/emperror"
	"github.com/lynx-go/x/log"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Hooks struct {
	hooks []Hook
}

func (hks *Hooks) Hooks() []Hook {
	return hks.hooks
}

func (hks *Hooks) Hook(hooks ...Hook) {
	hks.hooks = append(hks.hooks, hooks...)
}

func (hks *Hooks) OnStart(fns ...Func) {
	for _, fn := range fns {
		hook := &onStartHook{fn: fn}
		hks.hooks = append(hks.hooks, hook)
	}
}

func (hks *Hooks) OnStop(fns ...Func) {
	for _, fn := range fns {
		hook := &onStopHook{fn: fn}
		hks.hooks = append(hks.hooks, hook)
	}
}

type onStopHook struct {
	fn Func
}

func (h *onStopHook) Name() string {
	return "Stop"
}

func (h *onStopHook) Start(ctx context.Context) error {
	return nil
}

func (h *onStopHook) Stop(ctx context.Context) error {
	return h.fn(ctx)
}

var _ Hook = new(onStopHook)

type onStartHook struct {
	fn Func
}

func (h *onStartHook) Name() string {
	return "Start"
}

func (h *onStartHook) Start(ctx context.Context) error {
	return h.fn(ctx)
}

func (h *onStartHook) Stop(ctx context.Context) error {
	return nil
}

var _ Hook = new(onStartHook)

type Hook interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type Runnable func(ctx context.Context) error

func RunForever() Runnable {
	return func(ctx context.Context) error {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		return nil
	}
}

type SetupFunc[O any] func(ctx context.Context, hooks *Hooks, o O) (Runnable, error)

type App[O any] struct {
	onSetup SetupFunc[O]
	name    string
	version string
	hooks   *Hooks
	logger  *slog.Logger
}

func (sys *App[O]) Name() string {
	return sys.name
}

func (sys *App[O]) RunE(ctx context.Context, o O) error {
	sys.hooks = &Hooks{}
	var cancelCtx context.CancelFunc
	ctx, cancelCtx = context.WithCancel(ctx)
	defer cancelCtx()

	ctx = log.Context(ctx, sys.logger)
	runnable, err := sys.onSetup(ctx, sys.hooks, o)
	if err != nil {
		return err
	}
	eg, egCtx := errgroup.WithContext(ctx)
	wg := sync.WaitGroup{}
	for _, hook := range sys.hooks.Hooks() {
		eg.Go(func() error {
			<-egCtx.Done()
			stopCtx, cancelCtx := context.WithTimeout(egCtx, 5*time.Second)
			defer cancelCtx()
			return hook.Stop(stopCtx)
		})
		wg.Add(1)
		eg.Go(func() error {
			wg.Done()
			return hook.Start(ctx)
		})
	}
	wg.Wait()

	eg.Go(func() error {
		defer cancelCtx()
		return runnable(ctx)
	})
	return eg.Wait()
}

func (sys *App[O]) Run(ctx context.Context, o O) {
	emperror.Panic(sys.RunE(ctx, o))
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
		hooks: &Hooks{},
	}
	for _, opt := range opts {
		opt(app)
	}
	if app.logger == nil {
		app.logger = slog.Default().With("name", app.name, "version", app.version)
	}

	return app
}
