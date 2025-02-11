package lynx

import (
	"context"
	"emperror.dev/emperror"
	"github.com/lynx-go/lynx/run"
	"github.com/samber/lo/mutable"
	"github.com/spf13/cobra"
	"log/slog"
)

type App interface {
	Run()
	Context() context.Context
	//Root() *cobra.Command
}

var _ App = new(app)

type options struct {
	name     string
	id       string
	version  string
	onInit   func()
	onStarts []Hook
	onStops  []Hook
	logger   *slog.Logger
	servers  []Server
	commands []Command
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

func WithServer(servers ...Server) Option {
	return func(o *options) {
		if o.servers == nil {
			o.servers = []Server{}
		}
		o.servers = append(o.servers, servers...)
	}
}

func WithCommands(commands ...Command) Option {
	return func(o *options) {
		if o.commands == nil {
			o.commands = make([]Command, 0)
		}
		o.commands = append(o.commands, commands...)
	}
}

func WithOnInit(onInit func()) Option {
	return func(o *options) { o.onInit = onInit }
}

func New(opts ...Option) App {
	o := &options{}
	//basePath := filepath.Base(os.Args[0])
	for _, opt := range opts {
		opt(o)
	}
	if o.logger == nil {
		o.logger = slog.Default().With("name", o.name, "id", o.id, "version", o.version)
		slog.SetDefault(o.logger)
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
	cobra.OnInitialize(func() {
		if o.onInit != nil {
			o.onInit()
		}
	})

	runningServers := make([]Server, 0)
	a.root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) (err error) {
		for _, srv := range o.servers {
			if s, ok := srv.(NotForCLI); !ok || !s.NotForCLI() {
				ctx := cmd.Context()
				logger.Info("starting server", "server_name", srv.Name())
				if err = srv.Start(ctx); err != nil {
					logger.Error("server start failed", "server_name", srv.Name(), "error", err)
					return err
				}
				logger.Info("started server", "server_name", srv.Name())
				runningServers = append(runningServers, srv)
			}
		}

		for _, hook := range o.onStarts {
			ctx := cmd.Context()
			if err = hook(ctx); err != nil {
				logger.Error("call OnStart hook failed", "error", err)
				return err
			}
		}
		return
	}
	a.root.PreRunE = func(cmd *cobra.Command, _ []string) (err error) {
		for _, srv := range o.servers {
			if s, ok := srv.(NotForCLI); ok && s.NotForCLI() {
				ctx := cmd.Context()
				logger.Info("starting server", "server_name", srv.Name())
				if err = srv.Start(ctx); err != nil {
					logger.Error("server start failed", "server_name", srv.Name(), "error", err)
					return err
				}
				logger.Info("started server", "server_name", srv.Name())
				runningServers = append(runningServers, srv)
			}
		}

		return
	}

	a.root.PersistentPostRun = func(cmd *cobra.Command, _ []string) {
		for _, hook := range o.onStops {
			ctx := context.Background()
			if err := hook(ctx); err != nil {
				logger.Error("call OnStop hook failed", "error", err)
			}
		}
	}

	a.root.Run = func(cmd *cobra.Command, args []string) {
		run.ListenSignal()
	}

	for _, c := range a.o.commands {
		cmd := &cobra.Command{
			Use:   c.Name(),
			Short: c.Description(),
			Long:  c.Description(),
			RunE: func(cb *cobra.Command, args []string) error {
				return c.Command(cb.Context(), args)
			},
		}

		if srvs := c.Servers(); len(srvs) >= 0 {
			cmd.PreRunE = func(cb *cobra.Command, args []string) (err error) {
				for _, srv := range srvs {
					if s, ok := srv.(NotForCLI); !ok || !s.NotForCLI() {
						ctx := cmd.Context()
						logger.Info("starting server", "server_name", srv.Name())
						if err = srv.Start(ctx); err != nil {
							logger.Error("server start failed", "server_name", srv.Name(), "error", err)
							return err
						}
						logger.Info("started server", "server_name", srv.Name())
						runningServers = append(runningServers, srv)
					}
				}

				return
			}

		}
		a.root.AddCommand(cmd)
	}

	cobra.OnFinalize(func() {
		mutable.Reverse(runningServers)
		for _, srv := range runningServers {
			ctx := context.Background()
			logger.Info("stopping server", "server_name", srv.Name())
			if err := srv.Stop(ctx); err != nil {
				logger.Error("server stop failed", "server_name", srv.Name(), "error", err)
			}
			logger.Info("stopped server", "server_name", srv.Name())
		}
	})

	return a
}

func (a *app) Run() {
	emperror.Panic(a.root.Execute())
}
