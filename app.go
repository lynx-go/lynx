package lynx

import (
	"context"
	"emperror.dev/emperror"
	"github.com/spf13/cobra"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

type App interface {
	Run()
	Context() context.Context
}

type Hooks interface {
	OnStart(ctx context.Context) error
	OnStop(ctx context.Context) error
}

var _ App = new(app)

type options struct {
	bootstrap func(ctx context.Context, args []string) error
	name      string
	id        string
	version   string
	onStarts  []Hook
	onStops   []Hook
	logger    *slog.Logger
	services  []Service
}

type Hook func(ctx context.Context) error
type app struct {
	root *cobra.Command
	o    *options
}

func (a *app) Context() context.Context {
	return a.root.Context()
}

type Option func(*options)

func WithBoostrap(fn func(ctx context.Context, args []string) error) Option {
	return func(o *options) {
		o.bootstrap = fn
	}
}

func WithOnStart(hooks ...Hook) Option {
	return func(o *options) {
		if o.onStarts == nil {
			o.onStarts = make([]Hook, 0)
		}
		o.onStarts = append(o.onStarts, hooks...)
	}
}

func WithOnStop(hooks ...Hook) Option {
	return func(o *options) {
		if o.onStops == nil {
			o.onStops = make([]Hook, 0)
		}
		o.onStops = append(o.onStops, hooks...)
	}
}

func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

func WithID(id string) Option {
	return func(o *options) { o.id = id }
}

func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

func WithVersion(v string) Option {
	return func(o *options) { o.version = v }
}

func WithServices(services ...Service) Option {
	return func(o *options) {
		if o.onStarts == nil {
			o.services = []Service{}
		}
		o.services = append(o.services, services...)
	}

}

func New(opts ...Option) App {
	o := &options{
		logger: slog.Default(),
	}
	//basePath := filepath.Base(os.Args[0])
	for _, opt := range opts {
		opt(o)
	}
	logger := o.logger
	a := &app{
		o: o,
		root: &cobra.Command{
			Use:           o.name,
			Version:       o.version,
			SilenceErrors: true,
		},
	}

	runningServices := make([]Service, 0)
	a.root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) (err error) {
		//defer func() {
		//	recovered := recover()
		//	err = emperror.Recover(recovered)
		//}()
		for _, svc := range o.services {
			ctx := cmd.Context()
			if err = svc.Start(ctx); err != nil {
				logger.Error("service start failed", "service", svc.Name(), "error", err)
				return err
			}
			runningServices = append(runningServices, svc)
		}

		return
	}
	a.root.PreRunE = func(cmd *cobra.Command, _ []string) (err error) {
		for _, hook := range o.onStarts {
			ctx := cmd.Context()
			if err = hook(ctx); err != nil {
				logger.Error("call OnStart hook failed", "error", err)
				return err
			}
		}
		return
	}

	cobra.OnFinalize(func() {
		for _, svc := range runningServices {
			ctx := context.Background()
			if err := svc.Stop(ctx); err != nil {
				logger.Error("service stop failed", "service", svc.Name(), "error", err)
			}
		}
	})

	a.root.PersistentPostRun = func(cmd *cobra.Command, _ []string) {
		for _, hook := range o.onStops {
			ctx := context.Background()
			if err := hook(ctx); err != nil {
				logger.Error("call OnStop hook failed", "error", err)
			}
		}
	}

	a.root.Run = func(cmd *cobra.Command, args []string) {
		// Handle graceful shutdown.
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-quit:
		}
	}

	return a
}

func (a *app) Run() {
	emperror.Panic(a.root.Execute())
}
