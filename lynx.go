package lynx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"

	"github.com/lynx-go/x/log"
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
	// Hooks 添加 OnStart/OnStop/Component/ComponentBuilder Hooks
	Hooks(hooks ...HookOption) error

	// HealthCheckFunc 注册到 HTTP 的 Health Check 方法
	HealthCheckFunc() HealthCheckFunc
	// Run 启用 CLI
	Run() error
	// SetLogger 设置 logger
	SetLogger(logger *slog.Logger)
	// Logger 获取 logger
	Logger(kwargs ...any) *slog.Logger
	//Hooks
}

type nameCtx struct{}

var keyName = nameCtx{}

type idCtx struct{}

var keyId = idCtx{}

type versionCtx struct{}

var keyVersion = versionCtx{}

// IDFromContext returns the instance ID from the context.
// Returns an empty string if the ID is not set or has wrong type.
func IDFromContext(ctx context.Context) string {
	if v := ctx.Value(keyId); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// VersionFromContext returns the application version from the context.
// Returns an empty string if the version is not set or has wrong type.
func VersionFromContext(ctx context.Context) string {
	if v := ctx.Value(keyVersion); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// NameFromContext returns the application name from the context.
// Returns an empty string if the name is not set or has wrong type.
func NameFromContext(ctx context.Context) string {
	if v := ctx.Value(keyName); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

type lynx struct {
	*hooks
	mu              sync.Mutex
	o               *Options
	f               *pflag.FlagSet
	c               *viper.Viper
	ctx             context.Context
	cancelCtx       context.CancelFunc
	runG            *run.Group
	logger          *slog.Logger
	healthCheckers  []health.Checker
}

func (app *lynx) Hooks(hooks ...HookOption) error {
	app.mu.Lock()
	defer app.mu.Unlock()

	options := &hookOptions{}
	for _, hook := range hooks {
		hook(options)
	}

	app.hooks.onStarts = append(app.hooks.onStarts, options.onStarts...)
	app.hooks.onStops = append(app.hooks.onStops, options.onStops...)
	if err := app.addComponents(options.components...); err != nil {
		return err
	}

	if err := app.addComponentBuilders(options.componentBuilders...); err != nil {
		return err
	}
	return nil
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
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.addComponents(NewCommand(cmd))
}

func (app *lynx) Close() {
	app.cancelCtx()
}

func (app *lynx) init() error {
	if err := app.initConfigure(); err != nil {
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
	f.StringP("config", "c", "", "config file path")
	f.String("config-type", "yaml", "config file type, default yaml")
	f.String("config-dir", "", "config file path")
	f.String("log-level", "info", "log level, default info")
}

func DefaultBindConfigFunc(f *pflag.FlagSet, v *viper.Viper) error {
	if c, _ := f.GetString("config"); c != "" {
		v.SetConfigFile(c)
	}
	if cd, _ := f.GetString("config-dir"); cd != "" {
		v.AddConfigPath(cd)
	}
	if t, _ := f.GetString("config-type"); t != "" {
		v.SetConfigType(t)
	}
	return nil
}

func (app *lynx) initConfigure() error {
	if fn := app.o.SetFlagsFunc; fn != nil {
		fn(app.f)
		if err := app.f.Parse(os.Args[1:]); err != nil {
			return fmt.Errorf("failed to parse flags: %w", err)
		}
	}

	if fn := app.o.BindConfigFunc; fn != nil {
		if err := fn(app.f, app.c); err != nil {
			return err
		}

		if err := app.c.ReadInConfig(); err != nil {
			return fmt.Errorf("failed to read config: %w", err)
		}
	}

	if app.o.SetFlagsFunc != nil {
		if err := app.c.BindPFlags(app.f); err != nil {
			return fmt.Errorf("failed to bind flags: %w", err)
		}
	}

	return nil
}

func (app *lynx) addComponentBuilders(builders ...ComponentBuilder) error {

	for _, builder := range builders {
		build := builder.Build
		options := builder.Options()
		options.ensureDefaults()
		var components []Component
		for i := 0; i < options.Instances; i++ {
			comp := build()
			components = append(components, comp)
		}
		if err := app.addComponents(components...); err != nil {
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

func (app *lynx) addComponents(components ...Component) error {
	for _, component := range components {
		ctx, cancel := context.WithCancel(context.Background())
		log.InfoContext(ctx, "initializing component", "component", component.Name())
		if err := component.Init(app); err != nil {
			cancel()
			return err
		}
		log.InfoContext(ctx, "initialized component", "component", component.Name())
		app.runG.Add(func() error {
			log.InfoContext(ctx, "starting component", "component", component.Name())
			return component.Start(ctx)
		}, func(err error) {
			log.InfoContext(ctx, "stopping component", "component", component.Name())
			component.Stop(ctx)
			cancel()
		})
		if hc, ok := component.(health.Checker); ok {
			app.healthCheckers = append(app.healthCheckers, hc)
		}
	}
	return nil
}

func (app *lynx) Run() error {
	app.Logger().Info("starting")
	app.runG.Add(func() error {
		app.Logger().Info("run on-start hooks")
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

	closeTimeout := app.o.CloseTimeout
	app.runG.Add(func() error {
		exitCh := make(chan os.Signal, 1)
		signal.Notify(exitCh, app.o.ExitSignals...)
		select {
		case <-app.ctx.Done():
			return nil
		case <-exitCh:
			return nil
		}
	}, func(err error) {
		app.Logger().Info("shutting down")

		// Step 1: Cancel context first to signal components to stop
		app.cancelCtx()

		// Step 2: Execute OnStop hooks with timeout
		ctx, cancelCtx := context.WithTimeout(context.Background(), closeTimeout)
		defer cancelCtx()
		app.Logger().Info("run on-stop hooks")
		var shutdownErrors ShutdownErrors
		for _, fn := range app.hooks.onStops {
			fn := fn
			if hookErr := fn(ctx); hookErr != nil {
				app.logger.ErrorContext(ctx, "on-stop hook called error", "error", hookErr)
				shutdownErrors.Add(hookErr)
			}
		}
		if shutdownErrors.HasErrors() {
			app.logger.ErrorContext(ctx, "shutdown completed with errors", "errors", shutdownErrors.Error())
		}
		// Step 3: run.Group will automatically stop all components after this
	})
	return app.runG.Run()
}

func newLynx(o *Options) (Lynx, error) {
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
		return nil, err
	}
	return app, nil
}
