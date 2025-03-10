package lynx

import (
	"context"
	"emperror.dev/emperror"
	"fmt"
	"github.com/lynx-go/lynx/hook"
	"github.com/lynx-go/lynx/run"
	"github.com/lynx-go/x/log"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"sync"
	"time"
)

type Meta struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (md Meta) String() string {
	return fmt.Sprintf("%s/%s/%s", md.ID, md.Name, md.Version)
}

func (md Meta) UniqID() string {
	return fmt.Sprintf("%s/%s", md.Name, md.ID)
}

type SetupFunc[O any] func(ctx context.Context, hooks *hook.Hooks, o O, args []string) (run.RunFunc, error)

type App[O any] struct {
	onSetup       SetupFunc[O]
	md            *Meta
	registrar     *hook.Hooks
	logger        *slog.Logger
	mux           sync.Mutex
	isInitialized bool
	isClosed      bool
}

func (app *App[O]) Name() string {
	return app.md.Name
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
			if err := hk.Start(ctx); err != nil {
				cancelCtx()
				return err
			}
			return nil
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

func WithMeta[O any](md *Meta) Option[O] {
	return func(a *App[O]) {
		a.md = md
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
	logger := app.logger
	if logger == nil {
		logger = slog.Default()
	}
	if app.logger == nil {
		app.logger = logger.With("service_id", app.md.ID, "service_name", app.md.Name, "service_version", app.md.Version)
	}

	return app
}
