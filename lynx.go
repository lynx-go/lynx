// Package lynx 是 Lynx 微服务框架的核心包：提供应用生命周期管理、
// 组件系统、Hooks 机制、配置管理与 Context 辅助函数。
package lynx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/oklog/run"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gocloud.dev/server/health"
)

// BindConfigFunc 将命令行 flags 绑定到应用配置源（ConfigSource 实例）。
type BindConfigFunc func(f *pflag.FlagSet, c ConfigSource) error

// SetFlagsFunc 定义应用启动时需要注册的命令行 flags。
type SetFlagsFunc func(f *pflag.FlagSet)

// App 是 App 应用实例的核心接口，提供配置、上下文、组件与生命周期管理能力。
type App interface {
	// Close 关闭应用实例
	Close()
	// Config 获取配置实例（默认由 *viper.Viper 适配实现）
	Config() Config
	// Context 获取应用上下文
	Context() context.Context
	// CLI 注册启动的命令，用于 CLI 模式
	CLI(cmd CommandFunc) error

	// OnStart 注册应用启动阶段执行的钩子函数
	OnStart(fns ...HookFunc)
	// OnStop 注册应用停止阶段执行的钩子函数
	OnStop(fns ...HookFunc)
	// Register 注册需要由应用托管生命周期的组件实例。
	// 组件的 Init 在注册时同步执行；注册阶段产生的错误不会立即返回，
	// 首个错误会被记录，并在 Run() 时统一返回。
	Register(components ...Component)
	// RegisterBuilders 注册需要由应用托管生命周期的组件构建器，
	// 错误处理语义与 Register 相同。
	RegisterBuilders(builders ...ComponentBuilder)

	// HealthCheckFunc 注册到 HTTP 的 Health Check 方法
	HealthCheckFunc() HealthCheckFunc
	// Run 运行应用主流程：执行 on-start 钩子、启动所有组件并等待退出信号
	Run() error
	// SetLogger 设置 logger
	SetLogger(logger *slog.Logger)
	// Logger 获取 logger
	Logger(kwargs ...any) *slog.Logger
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
	mu             sync.Mutex
	o              *Options
	f              *pflag.FlagSet
	c              *viper.Viper
	ctx            context.Context
	cancelCtx      context.CancelFunc
	runG           *run.Group
	logger         *slog.Logger
	healthCheckers []health.Checker
	// components 按注册顺序记录已 Init 成功的组件，用于失败路径的逆序清理。
	components []Component

	onStarts []HookFunc
	onStops  []HookFunc
	// initErr 记录注册阶段产生的首个错误，由 Run() 统一返回。
	initErr error
}

func (app *lynx) OnStart(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onStarts = append(app.onStarts, fns...)
}

func (app *lynx) OnStop(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onStops = append(app.onStops, fns...)
}

func (app *lynx) Register(components ...Component) {
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return
	}
	// addComponents 在锁外执行 Init：组件 Init 内调用 app.HealthCheckFunc()、
	// OnStart 等需要 app.mu 的方法时不会死锁。
	if err := app.addComponents(components...); err != nil {
		app.recordInitError(err)
		log.ErrorContext(app.ctx, "failed to register components", err)
	}
}

func (app *lynx) RegisterBuilders(builders ...ComponentBuilder) {
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return
	}
	if err := app.addComponentBuilders(builders...); err != nil {
		app.recordInitError(err)
		log.ErrorContext(app.ctx, "failed to register component builders", err)
	}
}

// recordInitError 记录注册阶段产生的首个错误；仅首个生效，后续错误被忽略。
func (app *lynx) recordInitError(err error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.initErr == nil {
		app.initErr = err
	}
}

func (app *lynx) SetLogger(logger *slog.Logger) {
	slog.SetDefault(logger)
	app.logger = logger
}

func (app *lynx) HealthCheckFunc() HealthCheckFunc {
	return func() []health.Checker {
		app.mu.Lock()
		defer app.mu.Unlock()
		out := make([]health.Checker, len(app.healthCheckers))
		copy(out, app.healthCheckers)
		return out
	}
}

func (app *lynx) CLI(cmd CommandFunc) error {
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return initErr
	}
	if err := app.addComponents(NewCommand(cmd)); err != nil {
		app.recordInitError(err)
		return err
	}
	return nil
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

	app.applyLogLevel()
	return nil
}

// applyLogLevel 读取配置中的日志级别（--log-level / log_level）并应用到
// 应用默认 logger 上。仅当用户未通过 SetLogger 覆盖时生效——init 在
// 构建回调之前运行，构建回调里的 SetLogger 会再次替换 app.logger。
func (app *lynx) applyLogLevel() {
	levelStr := app.c.GetString("log-level")
	if levelStr == "" {
		levelStr = app.c.GetString("log_level")
	}
	if levelStr == "" {
		levelStr = app.c.GetString("logging.level")
	}
	if levelStr == "" {
		return
	}
	level, err := parseLogLevel(levelStr)
	if err != nil {
		app.logger.Warn("invalid log level, using default", "level", levelStr)
		return
	}
	var levelVar slog.LevelVar
	levelVar.Set(level)
	app.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &levelVar}))
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("unrecognized log level %q", s)
}

// DefaultSetFlagsFunc 注册默认的命令行 flags：配置文件路径、类型、目录与日志级别。
func DefaultSetFlagsFunc(f *pflag.FlagSet) {
	f.StringP("config", "c", "", "config file path")
	f.String("config-type", "yaml", "config file type, default yaml")
	f.String("config-dir", "", "config file path")
	f.String("log-level", "info", "log level, default info")
}

// DefaultBindConfigFunc 将默认 flags 中的配置文件路径、目录与类型绑定到应用配置源。
func DefaultBindConfigFunc(f *pflag.FlagSet, c ConfigSource) error {
	if cf, _ := f.GetString("config"); cf != "" {
		c.SetFile(cf)
	}
	if cd, _ := f.GetString("config-dir"); cd != "" {
		c.AddSearchPath(cd)
	}
	if t, _ := f.GetString("config-type"); t != "" {
		c.SetFileFormat(t)
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
		if err := fn(app.f, NewViperConfig(app.c)); err != nil {
			return err
		}

		if err := app.c.ReadInConfig(); err != nil {
			// 未显式指定配置文件且搜索路径下也不存在配置文件时，配置是可选的，
			// 不应阻止应用启动。只有显式指定的文件（如 -c missing.yaml）或
			// 解析错误才是硬失败。
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				return fmt.Errorf("failed to read config: %w", err)
			}
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

func (app *lynx) Config() Config {
	return NewViperConfig(app.c)
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
		// 组件上下文携带应用元数据（name/id/version），但不继承取消信号：
		// 组件仍由 run.Group 中断（Stop + cancel）来停止，从而保证关闭时
		// OnStop hooks 先于组件 Stop 执行。
		ctx, cancel := context.WithCancel(context.WithoutCancel(app.ctx))
		log.InfoContext(ctx, "initializing component", "component", component.Name())
		// Init 在锁外执行（调用方不持 app.mu）：Init 内调用 app.HealthCheckFunc()
		// 等需要 app.mu 的方法时不会死锁。
		if err := component.Init(app); err != nil {
			cancel()
			// 逆序有界停止本批及此前已 Init 成功的组件，释放其打开的资源。
			app.stopComponents(app.ctx)
			return err
		}
		log.InfoContext(ctx, "initialized component", "component", component.Name())
		app.mu.Lock()
		app.components = append(app.components, component)
		app.runG.Add(func() error {
			log.InfoContext(ctx, "starting component", "component", component.Name())
			return component.Start(ctx)
		}, func(err error) {
			log.InfoContext(ctx, "stopping component", "component", component.Name())
			// cancel 在 Stop 之后执行：Stop 收到的 ctx 在 Stop 期间保持存活，
			// 组件可用它作为优雅关停的宽限期（如 HTTP 的 Shutdown）。
			// 挂死（如等待 ctx.Done()）的 Stop 由 StopTimeout 有界兜底。
			app.stopComponentBounded(ctx, component)
			cancel()
		})
		if hc, ok := component.(health.Checker); ok {
			app.healthCheckers = append(app.healthCheckers, hc)
		}
		app.mu.Unlock()
	}
	return nil
}

// stopComponentBounded 有界停止单个组件：超过 StopTimeout 后记录错误并继续，
// 防止挂死的组件 Stop 阻塞整个关停流程。
func (app *lynx) stopComponentBounded(ctx context.Context, component Component) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		component.Stop(ctx)
	}()
	select {
	case <-done:
	case <-time.After(app.o.StopTimeout):
		app.logger.ErrorContext(app.ctx, "component stop timed out",
			"component", component.Name(), "timeout", app.o.StopTimeout.String())
	}
}

// stopComponents 逆序停止已注册组件，用于 Init/OnStart 失败路径的资源清理。
func (app *lynx) stopComponents(ctx context.Context) {
	app.mu.Lock()
	comps := append([]Component(nil), app.components...)
	app.mu.Unlock()
	for i := len(comps) - 1; i >= 0; i-- {
		app.stopComponentBounded(ctx, comps[i])
	}
}

func (app *lynx) Run() error {
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return initErr
	}
	app.Logger().Info("starting")

	// 退出信号提前注册：OnStart hook 阻塞期间收到的信号进入缓冲 chan，
	// hook 结束后立即触发关停——此前信号注册在 hook 之后，阻塞的 hook
	// 会使进程对 SIGTERM/SIGINT 无响应。
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh, app.o.ExitSignals...)
	defer signal.Stop(exitCh)

	// 顺序执行 OnStart hooks，全部成功后组件才开始启动。
	if err := app.runOnStartHooks(); err != nil {
		// 未进入 run.Group：已 Init 的组件需手动逆序清理，释放资源。
		app.stopComponents(app.ctx)
		return err
	}

	// 关闭 actor：收到退出信号或应用上下文被取消时，先在 actor 内执行
	// OnStop hooks，返回后 run.Group 才按注册顺序停止组件——保证清理逻辑
	//（如从服务发现注销）发生在组件仍在服务期间。OnStop 错误随 Run() 上抛，
	// 让调用方（如 K8s）感知关停失败。
	var (
		shutdownOnce sync.Once
		shutdownErr  error
	)
	shutdown := func() {
		app.Logger().Info("shutting down")
		// Step 1: 取消应用上下文，通知组件开始收尾。
		app.cancelCtx()
		// Step 2: 在 ShutdownTimeout 内执行 OnStop hooks。
		shutdownErr = app.runOnStopHooks()
	}
	app.runG.Add(func() error {
		select {
		case <-app.ctx.Done():
			shutdownOnce.Do(shutdown)
			return shutdownErr
		case <-exitCh:
			shutdownOnce.Do(shutdown)
			return shutdownErr
		}
	}, func(err error) {
		app.Close()
		shutdownOnce.Do(shutdown)
	})

	// Step 3: run.Group 在第一个 actor 返回后停止所有组件。
	return app.runG.Run()
}

func (app *lynx) runOnStartHooks() error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onStarts...)
	app.mu.Unlock()

	app.Logger().Info("run on-start hooks")
	for _, fn := range hooks {
		if err := fn(app.ctx); err != nil {
			return err
		}
	}
	return nil
}

// runOnStopHooks 在 ShutdownTimeout 内顺序执行所有 OnStop hooks。
// 单个 hook 阻塞不会挂起整个关闭流程：超过时限后记录错误并继续。
// 收集到的错误（含超时）以 *ShutdownErrors 返回，由 Run() 上抛给调用方。
func (app *lynx) runOnStopHooks() error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onStops...)
	app.mu.Unlock()

	app.Logger().Info("run on-stop hooks")
	ctx, cancel := context.WithTimeout(context.Background(), app.o.ShutdownTimeout)
	defer cancel()

	var shutdownErrors ShutdownErrors
	for _, fn := range hooks {
		if ctx.Err() != nil {
			shutdownErrors.Add(errors.New("shutdown timeout exceeded while running on-stop hooks"))
			break
		}
		fn := fn
		done := make(chan error, 1)
		go func() { done <- fn(ctx) }()
		select {
		case hookErr := <-done:
			if hookErr != nil {
				app.logger.ErrorContext(app.ctx, "on-stop hook called error", "error", hookErr)
				shutdownErrors.Add(hookErr)
			}
		case <-ctx.Done():
			app.logger.ErrorContext(app.ctx, "on-stop hook did not complete within shutdown timeout")
			shutdownErrors.Add(errors.New("on-stop hook timed out"))
		}
	}
	if shutdownErrors.HasErrors() {
		app.logger.ErrorContext(app.ctx, "shutdown completed with errors", "errors", shutdownErrors.Error())
		return &shutdownErrors
	}
	return nil
}

func newLynx(o *Options) (App, error) {
	o.EnsureDefaults()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	app := &lynx{
		o:        o,
		c:        viper.New(),
		f:        pflag.NewFlagSet(os.Args[0], pflag.ContinueOnError),
		runG:     &run.Group{},
		logger:   slog.Default(),
		onStarts: []HookFunc{},
		onStops:  []HookFunc{},
	}
	app.ctx, app.cancelCtx = context.WithCancel(context.Background())
	app.components = []Component{}
	if err := app.init(); err != nil {
		return nil, err
	}
	return app, nil
}
