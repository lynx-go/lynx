package lynx

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/run"
	"gocloud.dev/server/health"
)

type Lynx interface {
	Init() error
	Close()
	Config() Configurer
	Option() *Options
	Context() context.Context
	// CLI 注册启动的命令，用于 CLI 模式
	CLI(cmd CommandFunc) error
	// Inject 加载组件，但只有当应用启动后才会执行 Start
	Inject(components ...Component) error
	InjectFactory(factories ...ComponentFactory) error
	HealthCheckFunc() HealthCheckFunc
	// Run 启用 App
	Run() error
	Hooks() Hooks
	SetLogger(logger *slog.Logger)
	Logger(kwargs ...any) *slog.Logger
}

type lynx struct {
	ctx            context.Context
	cancelCtx      context.CancelFunc
	o              Options
	runG           *run.Group
	hooks          *hooks
	logger         *slog.Logger
	c              Configurer
	healthCheckers []health.Checker
}

func (app *lynx) SetLogger(logger *slog.Logger) {
	slog.SetDefault(logger)
	app.logger = logger
}

func (app *lynx) HealthCheckFunc() HealthCheckFunc {
	return func() []health.Checker {
		return app.healthCheckers
	}
}

func (app *lynx) CLI(cmd CommandFunc) error {
	return app.Inject(NewCommand(cmd))
}

func (app *lynx) Close() {
	app.cancelCtx()
}

func (app *lynx) Init() error {
	app.initConfigurer()
	app.initLogger()
	return nil
}

func (app *lynx) initLogger() {
	app.logger = slog.Default()
}

func (app *lynx) initConfigurer() {
	configDir := app.o.ConfigDir
	config := app.o.Config
	configType := "yaml"
	if app.o.ConfigType != "" {
		configType = app.o.ConfigType
	}
	if configDir != "" {

		configDirs := strings.Split(configDir, ",")
		app.c = newConfigFromDir(configDirs, configType)
	} else if config != "" {
		app.c = newConfigFromFile(config)
	} else {
		app.c = newConfigFromDir([]string{"./configs"}, configType)
	}

	app.c.Merge(app.o.PropertiesAsMap())
}

func (app *lynx) InjectFactory(producers ...ComponentFactory) error {
	for _, producer := range producers {
		produce := producer.Component
		options := producer.Option()
		options.ensureDefaults()
		var components []Component
		for i := 0; i < options.Instances; i++ {
			comp := produce()
			components = append(components, comp)
		}
		if err := app.Inject(components...); err != nil {
			return err
		}
	}
	return nil
}

func (app *lynx) Config() Configurer {
	return app.c
}

func (app *lynx) Logger(kwargs ...any) *slog.Logger {
	return app.logger.With(kwargs...)
}

func (app *lynx) Context() context.Context {
	return app.ctx
}

func (app *lynx) Option() *Options {
	return &app.o
}

func (app *lynx) Inject(components ...Component) error {
	for _, comp := range components {
		ctx, cancel := context.WithCancel(context.Background())
		if err := comp.Init(app); err != nil {
			cancel()
			return err
		}
		app.runG.Add(func() error {
			return comp.Start(ctx)
		}, func(err error) {
			comp.Stop(ctx)
			cancel()
		})
		if hc, ok := comp.(health.Checker); ok {
			app.healthCheckers = append(app.healthCheckers, hc)
		}
	}
	return nil
}

func (app *lynx) Run() error {
	app.Logger().Info("starting")
	app.runG.Add(func() error {
		app.Logger().Info("run OnStart hooks")
		for _, fn := range app.hooks.onStarts {
			if err := fn(app.ctx); err != nil {
				return err
			}
		}
		select {
		case <-app.ctx.Done():
			return nil
		}
	}, func(err error) {
		app.Close()
	})

	closeTimeout := 10 * time.Second
	if app.c.GetInt("shutdown_timeout") > 0 {
		closeTimeout = time.Duration(app.c.GetInt("shutdown_timeout")) * time.Second
	}

	app.runG.Add(func() error {
		exit := make(chan os.Signal, 1)
		signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-app.ctx.Done():
			return nil
		case <-exit:
			return nil
		}
	}, func(err error) {
		app.Logger().Info("shutting down")
		ctx, cancelCtx := context.WithTimeout(context.TODO(), closeTimeout)
		defer cancelCtx()
		app.Logger().Info("run OnStop hooks")
		for _, fn := range app.hooks.onStops {
			fn := fn
			if err := fn(ctx); err != nil {
				app.logger.ErrorContext(app.ctx, "post stop func called error", "error", err)
			}
		}
	})
	return app.runG.Run()
}

func (app *lynx) Hooks() Hooks {
	return app.hooks
}

func newLynx(o Options) Lynx {
	o.EnsureDefaults()
	app := &lynx{
		o:    o,
		runG: &run.Group{},
		hooks: &hooks{
			onStarts: []HookFunc{},
			onStops:  []HookFunc{},
		},
	}
	app.ctx, app.cancelCtx = context.WithCancel(context.Background())
	if err := app.Init(); err != nil {
		log.Fatal(err)
	}
	return app
}
