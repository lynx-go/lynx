package lynx

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oklog/run"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gocloud.dev/server/health"
)

type BindConfigFunc func(f *pflag.FlagSet, v *viper.Viper) error
type SetFlagsFunc func(f *pflag.FlagSet)

type Lynx interface {
	// Close 关闭应用实例
	Close()
	// Config 获取配置实例
	Config() *viper.Viper
	// Context 获取应用上下文
	Context() context.Context
	// CLI 注册启动的命令，用于 CLI 模式
	CLI(cmd CommandFunc) error
	// Hook 加载组件，但只有当应用启动后才会执行 Start
	Hook(components ...Component) error
	// HookFactory 把 ComponentFactory 注入应用中
	HookFactory(factories ...ComponentFactory) error
	// HealthCheckFunc 注册到 HTTP 的 Health Check 方法
	HealthCheckFunc() HealthCheckFunc
	// Run 启用 App
	Run() error
	// SetLogger 设置 logger
	SetLogger(logger *slog.Logger)
	// Logger 获取 logger
	Logger(kwargs ...any) *slog.Logger
	Hooks
}

type nameCtx struct{}

var keyName = nameCtx{}

type idCtx struct{}

var keyId = idCtx{}

type versionCtx struct{}

var keyVersion = versionCtx{}

func IDFromContext(ctx context.Context) string {
	return ctx.Value(keyId).(string)
}

func VersionFromContext(ctx context.Context) string {
	return ctx.Value(keyVersion).(string)
}

func NameFromContext(ctx context.Context) string {
	return ctx.Value(keyName).(string)
}

type lynx struct {
	*hooks
	o              *Options
	f              *pflag.FlagSet
	c              *viper.Viper
	ctx            context.Context
	cancelCtx      context.CancelFunc
	runG           *run.Group
	logger         *slog.Logger
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
	return app.Hook(NewCommand(cmd))
}

func (app *lynx) Close() {
	app.cancelCtx()
}

func (app *lynx) init() error {
	if err := app.loadConfig(); err != nil {
		return err
	}

	name := app.c.GetString("name")
	if name == "" {
		name = app.o.Name
	}
	app.ctx = context.WithValue(app.ctx, keyName, name)
	id := app.c.GetString("id")
	if id == "" {
		id = app.o.ID
	}
	app.ctx = context.WithValue(app.ctx, keyId, id)
	version := app.c.GetString("version")
	if version == "" {
		version = app.o.Version
	}
	app.ctx = context.WithValue(app.ctx, keyVersion, version)

	return nil
}

func DefaultSetFlagsFunc(f *pflag.FlagSet) {
	f.StringP("config", "c", "./config.yaml", "config file path, default is ./configs")
	f.String("config_type", "yaml", "config file type, default yaml")
	f.String("config_dir", "", "config file path, default is ./configs")
	f.String("log_level", "info", "log level, default info")
}

func DefaultBindConfigFunc(f *pflag.FlagSet, v *viper.Viper) error {
	if c, _ := f.GetString("config"); c != "" {
		v.SetConfigFile(c)
	}
	if cd, _ := f.GetString("config_dir"); cd != "" {
		v.AddConfigPath(cd)
	}
	if t, _ := f.GetString("config_type"); t != "" {
		v.SetConfigType(t)
	}
	return nil
}

func (app *lynx) loadConfig() error {
	if fn := app.o.SetFlagsFunc; fn != nil {
		fn(app.f)
	}
	if err := app.f.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}

	if fn := app.o.BindConfigFunc; fn != nil {
		if err := fn(app.f, app.c); err != nil {
			return err
		}
	}

	if err := app.c.ReadInConfig(); err != nil {
		log.Fatal(err)
	}

	if err := app.c.BindPFlags(app.f); err != nil {
		log.Fatal(err)
	}
	return nil
}

func (app *lynx) HookFactory(producers ...ComponentFactory) error {
	for _, producer := range producers {
		produce := producer.Component
		options := producer.Option()
		options.ensureDefaults()
		var components []Component
		for i := 0; i < options.Instances; i++ {
			comp := produce()
			components = append(components, comp)
		}
		if err := app.Hook(components...); err != nil {
			return err
		}
	}
	return nil
}

func (app *lynx) Config() *viper.Viper {
	return app.c
}

func (app *lynx) Logger(kwargs ...any) *slog.Logger {
	return app.logger.With(kwargs...)
}

func (app *lynx) Context() context.Context {
	return app.ctx
}

func (app *lynx) Option() *Options {
	return app.o
}

func (app *lynx) Hook(components ...Component) error {
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

func newLynx(o *Options) Lynx {
	o.EnsureDefaults()
	app := &lynx{
		o:    o,
		c:    viper.New(),
		f:    pflag.CommandLine,
		runG: &run.Group{},
		hooks: &hooks{
			onStarts: []HookFunc{},
			onStops:  []HookFunc{},
		},
		logger: slog.Default(),
	}
	app.ctx, app.cancelCtx = context.WithCancel(context.Background())
	if err := app.init(); err != nil {
		log.Fatal(err)
	}
	return app
}
