package lynx

import (
	"context"
	"emperror.dev/emperror"
	"github.com/lynx-go/x/log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

type Hooks struct {
	hooks []Hook
}

func (hks *Hooks) Hooks() []Hook {
	return hks.hooks
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

func (hks *Hooks) Server(srvs ...Server) {
	for _, s := range srvs {
		hook := &serverHook{Server: s}
		hks.hooks = append(hks.hooks, hook)
	}
}

func (hks *Hooks) Service(svcs ...Service) {}

type onStopHook struct {
	fn Func
}

func (h *onStopHook) Name() string {
	return "OnStop"
}

func (h *onStopHook) OnStart(ctx context.Context) error {
	return nil
}

func (h *onStopHook) OnStop(ctx context.Context) error {
	return h.fn(ctx)
}

var _ Hook = new(onStopHook)

type onStartHook struct {
	fn Func
}

func (h *onStartHook) Name() string {
	return "OnStart"
}

func (h *onStartHook) OnStart(ctx context.Context) error {
	return h.fn(ctx)
}

func (h *onStartHook) OnStop(ctx context.Context) error {
	return nil
}

var _ Hook = new(onStartHook)

type serverHook struct {
	Server
	ctx       context.Context
	cancelCtx context.CancelFunc
}

func (s *serverHook) Name() string {
	return s.Server.Name()
}

func (s *serverHook) OnStart(ctx context.Context) error {
	s.ctx, s.cancelCtx = context.WithCancel(ctx)
	go func() {
		_ = s.Serve(s.ctx)
	}()
	return nil
}

func (s *serverHook) OnStop(ctx context.Context) error {
	s.cancelCtx()
	return s.Shutdown(ctx)
}

var _ Hook = new(serverHook)

type Hook interface {
	Name() string
	OnStart(ctx context.Context) error
	OnStop(ctx context.Context) error
}

type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type Server interface {
	Name() string
	Serve(ctx context.Context) error
	Shutdown(ctx context.Context) error
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

type System[O any] struct {
	onSetup SetupFunc[O]
	name    string
	version string
	hooks   *Hooks
	logger  *slog.Logger
}

func (sys *System[O]) RunE(ctx context.Context, o O) error {
	sys.hooks = &Hooks{}
	rctx := log.Context(ctx, sys.logger)
	runnable, err := sys.onSetup(rctx, sys.hooks, o)
	if err != nil {
		return err
	}
	return runnable(ctx)
}

func (sys *System[O]) Run(ctx context.Context, o O) {
	emperror.Panic(sys.RunE(ctx, o))
}

func (sys *System[O]) OnStart(ctx context.Context) error {
	hooks := sys.hooks.Hooks()
	for _, hook := range hooks {
		if err := hook.OnStart(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (sys *System[O]) OnStop(ctx context.Context) error {
	hooks := sys.hooks.Hooks()
	for _, hook := range hooks {
		if err := hook.OnStop(ctx); err != nil {
			return err
		}
	}
	return nil
}

func NewSystem[O any](name string, version string, setup SetupFunc[O]) *System[O] {

	logger := slog.Default().With("name", name, "version", version)
	return &System[O]{
		name:    name,
		version: version,
		onSetup: setup,
		logger:  logger,
	}
}
