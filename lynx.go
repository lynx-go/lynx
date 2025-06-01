package lynx

import (
	"context"
	"github.com/oklog/run"
	"gocloud.dev/server/health"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type Lynx interface {
	Init() error
	Close()
	Config() Configurer
	Option() *Options
	Context() context.Context
	// MainCommand 注册启动的命令，用于 CLI 模式
	MainCommand(cmd CommandFunc) error
	// Load 加载组件，但只有当应用启动后才会执行 Start
	Load(components ...Component) error
	LoadFromProducer(producers ...ComponentProducer) error
	HealthCheck() HealthCheckFunc
	Run() error
	Hooks() Hooks
	SetLogger(logger *slog.Logger)
	Logger(args ...any) *slog.Logger
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

func (lx *lynx) SetLogger(logger *slog.Logger) {
	slog.SetDefault(logger)
	lx.logger = logger
}

func (lx *lynx) HealthCheck() HealthCheckFunc {
	return func() []health.Checker {
		return lx.healthCheckers
	}
}

func (lx *lynx) MainCommand(cmd CommandFunc) error {
	return lx.Load(NewCommand(cmd))
}

func (lx *lynx) Close() {
	lx.cancelCtx()
}

func (lx *lynx) Init() error {
	lx.initConfigurer()
	lx.initLogger()
	return nil
}

func (lx *lynx) initLogger() {
	lx.logger = slog.Default()
}

func (lx *lynx) initConfigurer() {
	configDir := lx.o.ConfigDir
	config := lx.o.Config
	configType := "yaml"
	if lx.o.ConfigType != "" {
		configType = lx.o.ConfigType
	}
	if configDir != "" {

		configDirs := strings.Split(configDir, ",")
		lx.c = newConfigFromDir(configDirs, configType)
	} else if config != "" {
		lx.c = newConfigFromFile(config)
	} else {
		lx.c = newConfigFromDir([]string{"./configs"}, configType)
	}

	lx.c.Merge(lx.o.PropertiesAsMap())
}

func (lx *lynx) LoadFromProducer(producers ...ComponentProducer) error {
	for _, producer := range producers {
		produce := producer.Component
		options := producer.Option()
		options.ensureDefaults()
		var components []Component
		for i := 0; i < options.Instances; i++ {
			comp := produce()
			components = append(components, comp)
		}
		if err := lx.Load(components...); err != nil {
			return err
		}
	}
	return nil
}

func (lx *lynx) Config() Configurer {
	return lx.c
}

func (lx *lynx) Logger(args ...any) *slog.Logger {
	return lx.logger.With(args...)
}

func (lx *lynx) Context() context.Context {
	return lx.ctx
}

func (lx *lynx) Option() *Options {
	return &lx.o
}

func (lx *lynx) Load(components ...Component) error {
	for _, comp := range components {
		ctx, cancel := context.WithCancel(context.Background())
		if err := comp.Init(lx); err != nil {
			cancel()
			return err
		}
		lx.runG.Add(func() error {
			return comp.Start(ctx)
		}, func(err error) {
			comp.Stop(ctx)
			cancel()
		})
		if hc, ok := comp.(health.Checker); ok {
			lx.healthCheckers = append(lx.healthCheckers, hc)
		}
	}
	return nil
}

func (lx *lynx) Run() error {
	lx.Logger().Info("starting")
	lx.runG.Add(func() error {
		lx.Logger().Info("calling on start hooks")
		for _, fn := range lx.hooks.onStarts {
			if err := fn(lx.ctx); err != nil {
				return err
			}
		}
		select {
		case <-lx.ctx.Done():
			return nil
		}
	}, func(err error) {
		lx.Close()
	})

	lx.runG.Add(func() error {
		exit := make(chan os.Signal, 1)
		signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-lx.ctx.Done():
			return nil
		case <-exit:
			return nil
		}
	}, func(err error) {
		lx.Logger().Info("shutting down")
		ctx, cancelCtx := context.WithTimeout(context.TODO(), lx.o.ShutdownTimeout)
		defer cancelCtx()
		lx.Logger().Info("calling on stop hooks")
		for _, fn := range lx.hooks.onStops {
			fn := fn
			if err := fn(ctx); err != nil {
				lx.logger.ErrorContext(lx.ctx, "post stop func called error", "error", err)
			}
		}
	})
	return lx.runG.Run()
}

func (lx *lynx) Hooks() Hooks {
	return lx.hooks
}

func newLynx(o Options) Lynx {
	o.EnsureDefaults()
	lx := &lynx{
		o:    o,
		runG: &run.Group{},
		hooks: &hooks{
			onStarts: []HookFunc{},
			onStops:  []HookFunc{},
		},
	}
	lx.ctx, lx.cancelCtx = context.WithCancel(context.Background())
	if err := lx.Init(); err != nil {
		log.Fatal(err)
	}
	return lx
}
