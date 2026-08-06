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
	"sync/atomic"
	"time"

	"github.com/oklog/run"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// BindConfigFunc 将命令行 flags 绑定到应用配置源（ConfigSource 实例）。
type BindConfigFunc func(f *pflag.FlagSet, c ConfigSource) error

// SetFlagsFunc 定义应用启动时需要注册的命令行 flags。
type SetFlagsFunc func(f *pflag.FlagSet)

// App 是应用实例的核心接口：在 AppContext 的基础上增加组件注册、
// 生命周期钩子与运行控制能力。
type App interface {
	AppContext
	// Command 注册启动的命令，用于 CLI 模式
	Command(cmd CommandFunc) error

	// OnStart 注册应用启动阶段执行的钩子函数
	OnStart(fns ...HookFunc)
	// OnStop 注册应用停止阶段执行的钩子函数
	OnStop(fns ...HookFunc)
	// Register 注册需要由应用托管生命周期的组件实例。
	// 组件的 Init 在注册时同步执行；注册阶段产生的错误不会立即返回，
	// 首个错误会被记录，并在 Run() 时统一返回。
	// 所有注册必须先于 Run：Run 开始后调用将 panic（见 Run）。
	Register(components ...Component)
	// RegisterFactories 注册需要由应用托管生命周期的组件工厂，
	// 错误处理语义与 Register 相同；同样必须先于 Run。
	RegisterFactories(factories ...ComponentFactory)

	// Run 运行应用主流程：执行 on-start 钩子、启动所有组件并等待退出信号。
	// Run 开始后，Register/RegisterFactories 为禁止操作（panic），Command 返回错误。
	Run() error
	// SetLogger 设置 logger。注意：同时调用 slog.SetDefault 同步全局默认
	// logger，使进程内不经框架的裸 slog 调用（如 slog.Info）落到同一
	// logger——这是有意的全局副作用。
	SetLogger(logger *slog.Logger)
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
	healthCheckers []Checker
	// components 按注册顺序记录已 Init 成功的组件，用于失败路径的逆序清理。
	components []Component
	// running 标记 Run 已开始：此后 Register/RegisterFactories 为禁止操作，
	// Run 侧无需再与注册侧并发争用 run.G 的 actors。
	running atomic.Bool

	onStarts []HookFunc
	onStops  []HookFunc
	// initErr 记录注册阶段产生的首个错误，由 Run() 统一返回。
	initErr error
	// shutdownErrors 聚合组件 Stop 返回的错误与超时错误，由 Run() 统一上抛。
	shutdownErrors ShutdownErrors
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
	if app.running.Load() {
		// 所有注册必须先于 Run。Run 已开始的注册是编程错误，panic 明确
		// 报错（Register 无返回值，无法以错误返回）。
		panic("lynx: Register must not be called after Run() has started")
	}
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return
	}
	// addComponents 在锁外执行 Init：组件 Init 内调用 app.HealthCheckers()、
	// OnStart 等需要 app.mu 的方法时不会死锁。
	if err := app.addComponents(components...); err != nil {
		if errors.Is(err, errRunStarted) {
			// 与 Run 并发的迟到注册：持锁登记事务内的权威裁决点。
			panic("lynx: Register must not be called after Run() has started")
		}
		app.recordInitError(err)
		app.logger.ErrorContext(app.ctx, "failed to register components", "error", err)
	}
}

func (app *lynx) RegisterFactories(factories ...ComponentFactory) {
	if app.running.Load() {
		panic("lynx: RegisterFactories must not be called after Run() has started")
	}
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return
	}
	if err := app.addComponentFactories(factories...); err != nil {
		if errors.Is(err, errRunStarted) {
			panic("lynx: RegisterFactories must not be called after Run() has started")
		}
		app.recordInitError(err)
		app.logger.ErrorContext(app.ctx, "failed to register component factories", "error", err)
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

// errRunStarted 由 addComponents 在持锁登记事务中发现 Run 已开始时返回，
// 调用方（Register/RegisterFactories/Command）翻译为各自的明确错误/panic
//（所有注册必须先于 Run）。
var errRunStarted = errors.New("lynx: registration after Run() has started")

// SetLogger 设置 logger，并同步 slog.SetDefault 使全局默认 logger 与应用
// 一致（全局副作用见 App 接口注释）。
func (app *lynx) SetLogger(logger *slog.Logger) {
	slog.SetDefault(logger)
	app.logger = logger
}

// HealthCheckers 返回当前已注册的健康检查器快照。
func (app *lynx) HealthCheckers() []Checker {
	app.mu.Lock()
	defer app.mu.Unlock()
	out := make([]Checker, len(app.healthCheckers))
	copy(out, app.healthCheckers)
	return out
}

func (app *lynx) Command(cmd CommandFunc) error {
	if app.running.Load() {
		return errors.New("lynx: Command must not be called after Run() has started")
	}
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return initErr
	}
	if err := app.addComponents(NewCommand(cmd)); err != nil {
		if errors.Is(err, errRunStarted) {
			return errors.New("lynx: Command must not be called after Run() has started")
		}
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

	name := app.c.GetString("service.name")
	if name == "" {
		// 旧顶层键回退（过渡期，deprecated）。
		name = app.c.GetString("name")
	}
	if name == "" {
		name = app.o.Name
	}
	app.ctx = context.WithValue(app.ctx, keyName, name)
	id := app.c.GetString("service.id")
	if id == "" {
		id = app.c.GetString("id")
	}
	if id == "" {
		id = app.o.ID
	}
	app.ctx = context.WithValue(app.ctx, keyId, id)
	version := app.c.GetString("service.version")
	if version == "" {
		version = app.c.GetString("version")
	}
	if version == "" {
		version = app.o.Version
	}
	app.ctx = context.WithValue(app.ctx, keyVersion, version)

	app.applyLogLevel()
	return nil
}

// applyLogLevel 读取配置中的日志级别并应用到应用默认 logger 上。
// 仅当用户未通过 SetLogger 覆盖时生效——init 在构建回调之前运行，
// 构建回调里的 SetLogger 会再次替换 app.logger。
func (app *lynx) applyLogLevel() {
	levelStr := LogLevelFromConfig(app.Config())
	if levelStr == "" {
		return
	}
	level, err := ParseLogLevel(levelStr)
	if err != nil {
		app.logger.Warn("invalid log level, using default", "level", levelStr)
		return
	}
	var levelVar slog.LevelVar
	levelVar.Set(level)
	app.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &levelVar}))
	// 与应用日志保持单通道：--log-level 对框架与应用日志一致生效。
	slog.SetDefault(app.logger)
}

// LogLevelFromConfig 从配置中解析日志级别字符串，优先级：
// logging.level（结构化配置）→ log-level → log_level（扁平键兼容回退）。
// 未设置任何键时返回空字符串。
func LogLevelFromConfig(c Config) string {
	for _, key := range []string{"logging.level", "log-level", "log_level"} {
		if lvl := c.GetString(key); lvl != "" {
			return lvl
		}
	}
	return ""
}

// ParseLogLevel 解析日志级别字符串为 slog.Level。
func ParseLogLevel(s string) (slog.Level, error) {
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
	if app.o.SetFlagsFunc != nil {
		app.o.SetFlagsFunc(app.f)
		if err := app.f.Parse(os.Args[1:]); err != nil {
			if errors.Is(err, pflag.ErrHelp) {
				// --help：usage 已由 pflag 输出，作为初始化错误返回，
				// 由 Builder.Run 以非零状态码退出。
				return err
			}
			return fmt.Errorf("failed to parse flags: %w", err)
		}
	}

	if app.o.BindConfigFunc != nil {
		if err := app.o.BindConfigFunc(app.f, NewViperConfig(app.c)); err != nil {
			return err
		}
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

	if app.o.SetFlagsFunc != nil {
		if err := app.c.BindPFlags(app.f); err != nil {
			return fmt.Errorf("failed to bind flags: %w", err)
		}
	}

	return nil
}

func (app *lynx) addComponentFactories(factories ...ComponentFactory) error {

	for _, factory := range factories {
		fn := factory.New
		options := factory.Options()
		options.ensureDefaults()
		var components []Component
		for i := 0; i < options.Instances; i++ {
			comp := fn()
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

func (app *lynx) addComponents(components ...Component) error {
	for _, component := range components {
		if component == nil {
			// plain nil 检查：误注册 nil 组件返回明确错误而非运行时 panic。
			// （typed-nil 无法在不使用反射的前提下完全防御，见各 contrib
			// NewFromConfig 返回 nil 不得注册的文档约定。）
			app.stopComponents(app.ctx)
			return errors.New("lynx: cannot register nil component")
		}
		// 组件上下文携带应用元数据（name/id/version），但不继承取消信号：
		// 组件仍由 run.Group 中断（Stop + cancel）来停止，从而保证关闭时
		// OnStop hooks 先于组件 Stop 执行。
		ctx, cancel := context.WithCancel(context.WithoutCancel(app.ctx))
		app.logger.InfoContext(ctx, "initializing component", "component", component.Name())
		// Init 在锁外执行（调用方不持 app.mu）：Init 内调用
		// app.HealthCheckers() 等需要 app.mu 的方法时不会死锁。
		if err := component.Init(app); err != nil {
			cancel()
			// 逆序有界停止本批及此前已 Init 成功的组件，释放其打开的资源。
			app.stopComponents(app.ctx)
			return err
		}
		app.logger.InfoContext(ctx, "initialized component", "component", component.Name())
		// 登记事务：running 检查与 runG.Add 同持 app.mu。这是与 Run 并发时
		// 的权威裁决点——Run 在持 mu 时置位 running，此处同样持 mu 判定+登记，
		// 任何迟到的 Add 都不可能越过该检查；检查失败时组件不进入
		// components/healthCheckers/runG（无孤儿）。
		app.mu.Lock()
		if app.running.Load() {
			app.mu.Unlock()
			cancel()
			return errRunStarted
		}
		app.components = append(app.components, component)
		app.runG.Add(func() error {
			app.logger.InfoContext(ctx, "starting component", "component", component.Name())
			return component.Start(ctx)
		}, func(err error) {
			app.logger.InfoContext(ctx, "stopping component", "component", component.Name())
			// cancel 在 Stop 之后执行：Stop 收到的 ctx 在 Stop 期间保持存活，
			// 组件可用它作为优雅关停的宽限期（如 HTTP 的 Shutdown）。
			// 挂死（如等待 ctx.Done()）的 Stop 由 StopTimeout 有界兜底。
			app.stopComponentBounded(ctx, component)
			cancel()
		})
		if hc, ok := component.(Checker); ok {
			app.healthCheckers = append(app.healthCheckers, hc)
		}
		app.mu.Unlock()
	}
	return nil
}

// stopComponentBounded 有界停止单个组件：超过 StopTimeout 后记录错误并继续，
// 防止挂死的组件 Stop 阻塞整个关停流程。
// 注意：超时后组件 Stop 仍在后台 goroutine 运行，若其永久阻塞则该 goroutine
// 随之泄漏（可接受的取舍——保证关停流程不被挂死优先）。
// 组件 Stop 返回的错误与超时错误写入 shutdownErrors，由 Run() 统一上抛，
// 使调用方（如 K8s）能感知组件级关停失败。
func (app *lynx) stopComponentBounded(ctx context.Context, component Component) {
	done := make(chan error, 1)
	go func() {
		done <- component.Stop(ctx)
	}()
	var stopErr error
	select {
	case stopErr = <-done:
	case <-time.After(app.o.StopTimeout):
		stopErr = fmt.Errorf("component %q stop timed out after %v", component.Name(), app.o.StopTimeout)
		app.logger.ErrorContext(app.ctx, "component stop timed out",
			"component", component.Name(), "timeout", app.o.StopTimeout.String())
	}
	if stopErr != nil {
		app.logger.ErrorContext(app.ctx, "component stop error",
			"component", component.Name(), "error", stopErr)
		app.shutdownErrors.Add(stopErr)
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
	// running 在持 app.mu 时置位——与 Register 侧持锁登记事务的 running
	// 检查互斥，形成"检查与 runG.Add 同事务"的闭合判定。
	// 同时作为 Run 的单次守卫：二次调用直接返回错误，组件不会被二次
	// Start/Stop。
	alreadyRunning := app.running.Swap(true)
	app.mu.Unlock()
	if initErr != nil {
		return initErr
	}
	if alreadyRunning {
		return errors.New("lynx: Run must not be called more than once")
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
	// 关闭 actor 的登记同样持 app.mu：保证所有 runG.Add 都在锁内完成，
	// runG.Run() 迭代 actors 前不存在并发 Add。oklog/run 的 Add 仅是切片
	// append，持锁调用不会死锁。
	app.mu.Lock()
	app.runG.Add(func() error {
		select {
		case <-app.ctx.Done():
			shutdownOnce.Do(shutdown)
			// 返回 nil：Run 的返回统一在下方用 errors.Join 聚合 shutdownErr，
			// 避免信号路径与组件失败路径出现重复/丢失。
			return nil
		case <-exitCh:
			shutdownOnce.Do(shutdown)
			return nil
		}
	}, func(err error) {
		app.Close()
		shutdownOnce.Do(shutdown)
	})
	app.mu.Unlock()

	// Step 3: run.Group 在第一个 actor 返回后停止所有组件。
	// 组件 Start 先失败时 oklog/run 只返回首个 actor 错误；此处把 run group
	// 错误、OnStop 钩子错误与组件 Stop 错误聚合后一并上抛（nil 安全）。
	runErr := app.runG.Run()
	if app.shutdownErrors.HasErrors() {
		return errors.Join(runErr, shutdownErr, &app.shutdownErrors)
	}
	return errors.Join(runErr, shutdownErr)
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
	f := pflag.NewFlagSet(os.Args[0], pflag.ContinueOnError)
	// 忽略未知 flag：go test 二进制自带的 -test.* 参数不应导致初始化失败。
	f.ParseErrorsAllowlist.UnknownFlags = true
	app := &lynx{
		o:        o,
		c:        viper.New(),
		f:        f,
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
