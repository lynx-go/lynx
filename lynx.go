// Package lynx 是 Lynx 微服务框架的核心包：提供应用生命周期管理、
// 服务系统、Hooks 机制、配置管理与 Context 辅助函数。
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

	"github.com/lynx-go/lynx/eventbus"
	"github.com/oklog/run"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// BindFlagsFunc 定义应用启动时需要绑定的命令行 flags。
type BindFlagsFunc func(f *pflag.FlagSet)

// BindConfigFunc 将命令行 flags 绑定到应用配置源（ConfigSource 实例）。
type BindConfigFunc func(f *pflag.FlagSet, c ConfigSource) error

// App 是应用实例的核心接口：在 AppContext 的基础上增加服务注册、
// 生命周期钩子与运行控制能力。
type App interface {
	AppContext
	// Command 注册启动的命令，用于 CLI 模式
	Command(cmd CommandFunc) error

	// OnStart 注册应用启动阶段执行的钩子函数
	OnStart(fns ...HookFunc)
	// OnDrain 注册排水阶段执行的钩子函数：关停时 drainChecker 置位后
	// 与 DrainTimeout 睡眠并发执行（如从服务目录注销），总预算为
	// DrainHookTimeout（默认 3 秒）。
	OnDrain(fns ...HookFunc)
	// OnStop 注册应用停止阶段执行的钩子函数
	OnStop(fns ...HookFunc)
	// Register 注册需要由应用托管生命周期的服务实例。
	// 服务的 Init 在注册时同步执行；注册阶段产生的错误不会立即返回，
	// 首个错误会被记录，并在 Run() 时统一返回。
	// 所有注册必须先于 Run：Run 开始后调用将 panic（见 Run）。
	Register(services ...Service)
	// RegisterFactories 注册需要由应用托管生命周期的服务工厂，
	// 错误处理语义与 Register 相同；同样必须先于 Run。
	RegisterFactories(factories ...ServiceFactory)

	// Run 运行应用主流程：执行 on-start 钩子、启动所有服务并等待退出信号。
	// Run 开始后，Register/RegisterFactories 为禁止操作（panic），Command 返回错误。
	Run() error
	// SetLogger 设置 logger。注意：同时调用 slog.SetDefault 同步全局默认
	// logger，使进程内不经框架的裸 slog 调用（如 slog.Info）落到同一
	// logger——这是有意的全局副作用。
	SetLogger(logger *slog.Logger)
}

type metaCtx struct{}

var keyMeta = metaCtx{}

// Metadata describes the application metadata carried in the context:
// name, instance ID and version.
type Metadata struct {
	// Name is the application name.
	Name string
	// ID is the instance ID (hostname by default).
	ID string
	// Version is the application version.
	Version string
}

// Meta returns the application metadata from the context.
// Fields are empty strings if not set or of wrong type.
func Meta(ctx context.Context) Metadata {
	if v := ctx.Value(keyMeta); v != nil {
		if m, ok := v.(Metadata); ok {
			return m
		}
	}
	return Metadata{}
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
	// services 按注册顺序记录已 Init 成功的服务，用于失败路径的逆序清理。
	services []Service
	// running 标记 Run 已开始：此后 Register/RegisterFactories 为禁止操作，
	// Run 侧无需再与注册侧并发争用 run.G 的 actors。
	running atomic.Bool

	onStarts []HookFunc
	// onDrains 是排水钩子：SetDraining 之后与 DrainTimeout 睡眠并发执行，
	// 独立预算 DrainHookTimeout（见 runOnDrainHooks）。
	onDrains []HookFunc
	onStops  []HookFunc
	// drain 是框架内部的排水检查器（见 drain.go）：DrainTimeout > 0 时由
	// newLynx 注册进 healthCheckers，关停时置位让 readiness 立即失败。
	// 手构的 lynx 实例（如测试辅助）可能为 nil，shutdown 路径需判空。
	drain *drainChecker
	// bus 是应用级消息总线（一等对象），始终可用（默认内存实现）。
	bus eventbus.Bus
	// initErr 记录注册阶段产生的首个错误，由 Run() 统一返回。
	initErr error
	// shutdownErrors 聚合服务 Stop 返回的错误与超时错误，由 Run() 统一上抛。
	shutdownErrors ShutdownErrors
}

func (app *lynx) OnStart(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onStarts = append(app.onStarts, fns...)
}

func (app *lynx) OnDrain(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onDrains = append(app.onDrains, fns...)
}

func (app *lynx) OnStop(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onStops = append(app.onStops, fns...)
}

func (app *lynx) Register(services ...Service) {
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
	// addServices 在锁外执行 Init：服务 Init 内调用 app.HealthCheckers()、
	// OnStart 等需要 app.mu 的方法时不会死锁。
	if err := app.addServices(services...); err != nil {
		if errors.Is(err, errRunStarted) {
			// 与 Run 并发的迟到注册：持锁登记事务内的权威裁决点。
			panic("lynx: Register must not be called after Run() has started")
		}
		app.recordInitError(err)
		app.logger.ErrorContext(app.ctx, "failed to register services", "error", err)
	}
}

func (app *lynx) RegisterFactories(factories ...ServiceFactory) {
	if app.running.Load() {
		panic("lynx: RegisterFactories must not be called after Run() has started")
	}
	app.mu.Lock()
	initErr := app.initErr
	app.mu.Unlock()
	if initErr != nil {
		return
	}
	if err := app.addServiceFactories(factories...); err != nil {
		if errors.Is(err, errRunStarted) {
			panic("lynx: RegisterFactories must not be called after Run() has started")
		}
		app.recordInitError(err)
		app.logger.ErrorContext(app.ctx, "failed to register service factories", "error", err)
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

// errRunStarted 由 addServices 在持锁登记事务中发现 Run 已开始时返回，
// 调用方（Register/RegisterFactories/Command）翻译为各自的明确错误/panic
// （所有注册必须先于 Run）。
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
	if err := app.addServices(NewCommand(cmd)); err != nil {
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

	meta := Metadata{
		Name:    app.c.GetString("service.name"),
		ID:      app.c.GetString("service.id"),
		Version: app.c.GetString("service.version"),
	}
	if meta.Name == "" {
		meta.Name = app.o.Name
	}
	if meta.ID == "" {
		meta.ID = app.o.ID
	}
	if meta.Version == "" {
		meta.Version = app.o.Version
	}
	app.ctx = context.WithValue(app.ctx, keyMeta, meta)

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

// DefaultBindFlagsFunc 绑定默认的命令行 flags：配置文件路径、类型、目录与日志级别。
func DefaultBindFlagsFunc(f *pflag.FlagSet) {
	f.StringP("config", "c", "", "config file path")
	f.String("config-type", "yaml", "config file type, default yaml")
	f.String("config-dir", "", "config file path")
	// 默认值为空而非 "info"：BindPFlags 会把未显式传入的 flag 默认值绑进
	// viper，若默认 "info"，LogLevelFromConfig 的优先级链（logging.level →
	// log-level → log_level）会永久短路在 log-level，配置文件里的
	// logging.level/log_level 永远不生效（回归：config.yaml 设 log_level
	// 无效）。空默认时未传 flag 即回退配置文件键，缺省仍为 info。
	f.String("log-level", "", "log level, default info")
}

// DefaultBindConfigFunc 将默认 flags 中的配置文件路径、目录与类型绑定到应用配置源。
func DefaultBindConfigFunc(f *pflag.FlagSet, c ConfigSource) error {
	if cf, _ := f.GetString("config"); cf != "" {
		c.SetFile(cf)
	} else if cd, _ := f.GetString("config-dir"); cd == "" {
		// 未显式指定配置文件或搜索目录时，把工作目录加入搜索路径。
		// viper v1.17+ 不再隐式搜索 "."（曾有的默认行为），不加则
		// 运行目录下的 config.yaml 不会被发现（回归：无参运行时
		// kafka 等配置段加载不到，Transport 不启用、路由静默回退）。
		c.AddSearchPath(".")
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
	if app.o.BindFlagsFunc != nil {
		app.o.BindFlagsFunc(app.f)
		if err := app.f.Parse(os.Args[1:]); err != nil {
			if errors.Is(err, pflag.ErrHelp) {
				// --help：usage 已由 pflag 输出，作为初始化错误返回，
				// 由 Runner.Run 以非零状态码退出。
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

	if app.o.BindFlagsFunc != nil {
		if err := app.c.BindPFlags(app.f); err != nil {
			return fmt.Errorf("failed to bind flags: %w", err)
		}
	}

	return nil
}

func (app *lynx) addServiceFactories(factories ...ServiceFactory) error {

	for _, factory := range factories {
		fn := factory.New
		options := factory.Options()
		options.ensureDefaults()
		var services []Service
		for i := 0; i < options.Instances; i++ {
			srv := fn()
			services = append(services, srv)
		}
		if err := app.addServices(services...); err != nil {
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

func (app *lynx) Bus() eventbus.Bus {
	return app.bus
}

// publishEvent 发布内建生命周期事件，失败仅记 debug 日志，不影响主流程。
func (app *lynx) publishEvent(topic string, payload any) {
	if app.bus == nil {
		return
	}
	// 使用携带 Meta 的 app.ctx 为底，但脱离取消，避免关停时事件被取消。
	ctx := context.Background()
	if app.ctx != nil {
		ctx = context.WithoutCancel(app.ctx)
	}
	if err := app.bus.Publish(ctx, topic, payload); err != nil {
		app.logger.DebugContext(ctx, "publish lifecycle event failed", "topic", topic, "error", err)
	}
}

func (app *lynx) addServices(services ...Service) error {
	for _, service := range services {
		if service == nil {
			// plain nil 检查：误注册 nil 服务返回明确错误而非运行时 panic。
			// （typed-nil 无法在不使用反射的前提下完全防御，见各 contrib
			// NewFromConfig 返回 nil 不得注册的文档约定。）
			app.stopServices(app.ctx)
			return errors.New("lynx: cannot register nil service")
		}
		// 服务上下文携带应用元数据（name/id/version），但不继承取消信号：
		// 服务仍由 run.Group 中断（Stop + cancel）来停止，从而保证关闭时
		// OnStop hooks 先于服务 Stop 执行。
		ctx, cancel := context.WithCancel(context.WithoutCancel(app.ctx))
		app.logger.InfoContext(ctx, "initializing service", "service", service.Name())
		// Init 在锁外执行（调用方不持 app.mu）：Init 内调用
		// app.HealthCheckers() 等需要 app.mu 的方法时不会死锁。
		if err := service.Init(app); err != nil {
			cancel()
			app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now(), Error: err.Error()})
			// 逆序有界停止本批及此前已 Init 成功的服务，释放其打开的资源。
			app.stopServices(app.ctx)
			return err
		}
		app.logger.InfoContext(ctx, "initialized service", "service", service.Name())
		app.publishEvent(eventbus.TopicServiceRegistered, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
		// 登记事务：running 检查与 runG.Add 同持 app.mu。这是与 Run 并发时
		// 的权威裁决点——Run 在持 mu 时置位 running，此处同样持 mu 判定+登记，
		// 任何迟到的 Add 都不可能越过该检查；检查失败时服务不进入
		// services/healthCheckers/runG（无孤儿）。
		app.mu.Lock()
		if app.running.Load() {
			app.mu.Unlock()
			cancel()
			return errRunStarted
		}
		app.services = append(app.services, service)
		app.runG.Add(func() error {
			app.logger.InfoContext(ctx, "starting service", "service", service.Name())
			app.publishEvent(eventbus.TopicServiceStarting, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			// 对阻塞式服务，Started 语义为“已进入运行”：在 Start 调用前即发布，订阅者可据此协同。
			// 若 Start 立即失败，随后会发布 Failed，订阅者将看到 Starting→Started→Failed 的时序。
			app.publishEvent(eventbus.TopicServiceStarted, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			err := service.Start(ctx)
			if err != nil {
				app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now(), Error: err.Error()})
			}
			return err
		}, func(err error) {
			app.logger.InfoContext(ctx, "stopping service", "service", service.Name())
			app.publishEvent(eventbus.TopicServiceStopping, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			app.stopServiceBounded(ctx, service)
			// stopServiceBounded 已将错误聚合到 shutdownErrors，此处仅发布 Stopped/Failed 供订阅者感知。
			if app.shutdownErrors.HasErrors() {
				// 无法精确判定本服务的 Stop 是否失败，统一发布 Stopped，错误细节可查日志/shutdownErrors。
			}
			app.publishEvent(eventbus.TopicServiceStopped, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			cancel()
		})
		if hc, ok := service.(Checker); ok {
			app.healthCheckers = append(app.healthCheckers, hc)
		}
		app.mu.Unlock()
	}
	return nil
}

// stopServiceBounded 有界停止单个服务：超过 StopTimeout 后记录错误并继续，
// 防止挂死的服务 Stop 阻塞整个关停流程。
// 注意：超时后服务 Stop 仍在后台 goroutine 运行，若其永久阻塞则该 goroutine
// 随之泄漏（可接受的取舍——保证关停流程不被挂死优先）。
// 服务 Stop 返回的错误与超时错误写入 shutdownErrors，由 Run() 统一上抛，
// 使调用方（如 K8s）能感知服务级关停失败。
func (app *lynx) stopServiceBounded(ctx context.Context, service Service) {
	done := make(chan error, 1)
	go func() {
		done <- service.Stop(ctx)
	}()
	var stopErr error
	select {
	case stopErr = <-done:
	case <-time.After(app.o.StopTimeout):
		stopErr = fmt.Errorf("service %q stop timed out after %v", service.Name(), app.o.StopTimeout)
		app.logger.ErrorContext(app.ctx, "service stop timed out",
			"service", service.Name(), "timeout", app.o.StopTimeout.String())
	}
	if stopErr != nil {
		app.logger.ErrorContext(app.ctx, "service stop error",
			"service", service.Name(), "error", stopErr)
		app.shutdownErrors.Add(stopErr)
	}
}

// stopServices 逆序停止已注册服务，用于 Init/OnStart 失败路径的资源清理。
func (app *lynx) stopServices(ctx context.Context) {
	app.mu.Lock()
	svcs := append([]Service(nil), app.services...)
	app.mu.Unlock()
	for i := len(svcs) - 1; i >= 0; i-- {
		app.stopServiceBounded(ctx, svcs[i])
	}
}

func (app *lynx) Run() error {
	app.mu.Lock()
	initErr := app.initErr
	// running 在持 app.mu 时置位——与 Register 侧持锁登记事务的 running
	// 检查互斥，形成"检查与 runG.Add 同事务"的闭合判定。
	// 同时作为 Run 的单次守卫：二次调用直接返回错误，服务不会被二次
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
	meta := Meta(app.ctx)
	app.publishEvent(eventbus.TopicAppStarting, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})

	// 退出信号提前注册：OnStart hook 阻塞期间收到的信号进入缓冲 chan，
	// hook 结束后立即触发关停——此前信号注册在 hook 之后，阻塞的 hook
	// 会使进程对 SIGTERM/SIGINT 无响应。
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh, app.o.ExitSignals...)
	defer signal.Stop(exitCh)

	// 顺序执行 OnStart hooks，全部成功后服务才开始启动。
	if err := app.runOnStartHooks(); err != nil {
		// 未进入 run.Group：已 Init 的服务需手动逆序清理，释放资源。
		app.stopServices(app.ctx)
		app.publishEvent(eventbus.TopicAppStopped, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
		return err
	}
	app.publishEvent(eventbus.TopicAppStarted, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})

	// 总线单独托管（不经 runG 的 last-actor 语义），确保 AppStopped 等收尾事件能在总线关闭前投递，
	// 且其他服务的 Stopping/Stopped 事件不因总线提前关闭而丢失。
	busCtx, busCancel := context.WithCancel(context.WithoutCancel(app.ctx))
	// 发布总线自身的 Starting/Started，订阅者可在 Service.Init 中已就绪。
	app.publishEvent(eventbus.TopicServiceStarting, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
	app.publishEvent(eventbus.TopicServiceStarted, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
	go func() {
		if err := app.bus.Start(busCtx); err != nil {
			app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now(), Error: err.Error()})
		}
	}()
	// 等待总线进入 Running（最多 1s），避免后续 AppStarted 等事件因订阅尚未就绪而丢失。
	for i := 0; i < 20; i++ {
		if app.bus.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 确保 Run 返回后总线最后关闭，且 Stopping/Stopped 在关闭前可投递。
	defer func() {
		app.publishEvent(eventbus.TopicServiceStopping, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
		app.stopServiceBounded(busCtx, busService{app.bus})
		app.publishEvent(eventbus.TopicServiceStopped, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
		busCancel()
	}()

	// 关闭 actor：收到退出信号或应用上下文被取消时，先在 actor 内执行关停
	// 流程：置位 drainChecker 后与排水睡眠并发执行 OnDrain hooks（如从服务
	// 发现注销，DrainHookTimeout 预算），随后执行 OnStop hooks，返回后
	// run.Group 才按注册顺序停止服务——保证清理逻辑发生在服务仍在服务期间。
	// OnDrain/OnStop 错误随 Run() 上抛，让调用方（如 K8s）感知关停失败。
	var (
		shutdownOnce sync.Once
		shutdownErr  error
		drainErr     error
	)
	shutdown := func() {
		app.Logger().Info("shutting down")
		app.publishEvent(eventbus.TopicAppStopping, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
		// Step 0: 排水窗口。置位 drainChecker 使 readiness 聚合立即失败
		//（LB 摘流），等待 DrainTimeout 窗口结束后才执行后续关停。
		// DrainTimeout 与 ShutdownTimeout 是两段独立预算。
		// 所有关停入口（信号/中断/Close）都经过本函数，排水窗口统一生效。
		if app.drain != nil {
			app.drain.SetDraining(true)
		}
		if app.o.DrainTimeout > 0 {
			app.publishEvent(eventbus.TopicDrainStarting, eventbus.DrainEvent{Timeout: app.o.DrainTimeout, Time: time.Now()})
		}
		// OnDrain 钩子（如从服务目录注销）与排水睡眠并发：置位后立即开始，
		// 独立预算 DrainHookTimeout。注册钩子后关停时长上界 =
		// max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout + 各服务
		// StopTimeout；无钩子时整段跳过，不增加任何等待。
		drainHooksDone := make(chan struct{})
		if app.hasDrainHooks() {
			go func() {
				defer close(drainHooksDone)
				drainErr = app.runOnDrainHooks()
			}()
		} else {
			close(drainHooksDone)
		}
		if app.o.DrainTimeout > 0 {
			app.Logger().Info("draining: readiness marked unhealthy, waiting for drain window",
				"drain_timeout", app.o.DrainTimeout.String())
			// 窗口不可被 ctx 取消打断：排水语义要求服务在窗口内保持运行，
			// 供在途请求收尾；DrainTimeout=0 时跳过（与 v1.0 一致）。
			time.Sleep(app.o.DrainTimeout)
			app.publishEvent(eventbus.TopicDrainCompleted, eventbus.DrainEvent{Timeout: app.o.DrainTimeout, Time: time.Now()})
		}
		// 等待 OnDrain 钩子收尾（受 DrainHookTimeout 约束，不会挂死）。
		<-drainHooksDone
		// Step 1: 取消应用上下文，通知服务开始收尾。
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
			// 避免信号路径与服务失败路径出现重复/丢失。
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

	// Step 3: run.Group 在第一个 actor 返回后停止所有服务。
	// 服务 Start 先失败时 oklog/run 只返回首个 actor 错误；此处把 run group
	// 错误、OnDrain/OnStop 钩子错误与服务 Stop 错误聚合后一并上抛（nil 安全）。
	runErr := app.runG.Run()
	app.publishEvent(eventbus.TopicAppStopped, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
	if app.shutdownErrors.HasErrors() {
		return errors.Join(runErr, shutdownErr, drainErr, &app.shutdownErrors)
	}
	return errors.Join(runErr, shutdownErr, drainErr)
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

// hasDrainHooks 报告是否注册了 OnDrain 钩子。无钩子时关停路径整段跳过
// 钩子执行，不增加任何等待（回归红线：默认关停上界与既有版本一致）。
func (app *lynx) hasDrainHooks() bool {
	app.mu.Lock()
	defer app.mu.Unlock()
	return len(app.onDrains) > 0
}

// runOnDrainHooks 在 DrainHookTimeout 内顺序执行所有 OnDrain hooks。
// 语义对齐 runOnStopHooks：单个 hook 阻塞不会挂起整个关闭流程，超时
// 记录错误并继续；钩子错误不打断排水。使用独立的超时 Context（Background
// 派生），不传没有 deadline 的 app.ctx。
func (app *lynx) runOnDrainHooks() error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onDrains...)
	app.mu.Unlock()

	app.Logger().Info("run on-drain hooks")
	ctx, cancel := context.WithTimeout(context.Background(), app.o.DrainHookTimeout)
	defer cancel()

	var shutdownErrors ShutdownErrors
	for _, fn := range hooks {
		if ctx.Err() != nil {
			shutdownErrors.Add(errors.New("drain hook timeout exceeded while running on-drain hooks"))
			break
		}
		done := make(chan error, 1)
		go func() { done <- fn(ctx) }()
		select {
		case hookErr := <-done:
			if hookErr != nil {
				app.logger.ErrorContext(app.ctx, "on-drain hook called error", "error", hookErr)
				shutdownErrors.Add(hookErr)
			}
		case <-ctx.Done():
			app.logger.ErrorContext(app.ctx, "on-drain hook did not complete within drain hook timeout")
			shutdownErrors.Add(errors.New("on-drain hook timed out"))
		}
	}
	if shutdownErrors.HasErrors() {
		app.logger.ErrorContext(app.ctx, "drain hooks completed with errors", "errors", shutdownErrors.Error())
		return &shutdownErrors
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

// busService 是 eventbus.Bus 到 lynx.Service 的适配器：避免 bus 导入 lynx 导致的循环，
// 且不暴露 CheckHealth 到健康聚合（总线健康不影响 readiness）。
type busService struct{ b eventbus.Bus }

func (s busService) Name() string                        { return s.b.Name() }
func (s busService) Init(ctx AppContext) error           { return s.b.Init(ctx) }
func (s busService) Start(ctx context.Context) error     { return s.b.Start(ctx) }
func (s busService) Stop(ctx context.Context) error      { return s.b.Stop(ctx) }

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
		onDrains: []HookFunc{},
		onStops:  []HookFunc{},
		bus:      o.Bus,
	}
	app.ctx, app.cancelCtx = context.WithCancel(context.Background())
	app.services = []Service{}
	// drainChecker 仅当 DrainTimeout > 0 时注册进健康检查聚合：
	// DrainTimeout=0 时 healthCheckers 保持 nil，HealthCheckers() 快照
	// 内容与 v1.0 逐字节一致（回归红线）。
	app.drain = &drainChecker{}
	if o.DrainTimeout > 0 {
		app.healthCheckers = []Checker{app.drain}
	}
	if err := app.init(); err != nil {
		return nil, err
	}
	// 总线单独初始化（不经 addServices 的健康聚合），失败直接阻止启动；
	// 其 Start/Stop 由 Run 统一以 last-actor 语义托管，确保其他服务的 Stopping 事件能在总线关闭前投递。
	if err := app.bus.Init(app); err != nil {
		return nil, err
	}
	eventbus.SetDefault(app.bus)
	app.ctx = eventbus.ContextWithBus(app.ctx, app.bus)
	return app, nil
}
