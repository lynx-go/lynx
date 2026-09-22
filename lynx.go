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
//
// 钩子阶段按触发顺序（Pre/Post 锚定"服务组"的启停）：
//
//	OnPreStart  服务启动前，顺序执行，首错中止启动
//	OnPostStart 所有服务的 Start 已被调用后（阻塞型服务以进入 Start 为界）
//	OnDrain     关停发起、readiness 已摘流后，与排水窗口并发
//	OnPreStop   服务 Stop 之前（服务仍在服务在途请求）
//	OnPostStop  所有服务与总线停止之后、Run 返回前（进程收尾清理）
type App interface {
	AppContext
	// Command 注册启动的命令，用于 CLI 模式
	Command(cmd CommandFunc) error

	// OnPreStart 注册应用启动阶段执行的钩子函数：先于所有服务启动
	// 顺序执行，首个错误中止启动（已 Init 的服务逆序停止）。
	OnPreStart(fns ...HookFunc)
	// OnDrain 注册排水阶段执行的钩子函数：关停时 drainChecker 置位后
	// 与 DrainTimeout 窗口睡眠并发执行（如从服务目录注销），窗口即
	// 钩子总预算——窗口结束（无论钩子是否完成）即继续后续关停。
	// 必须启用排水（WithDrainTimeout，> 0）：DrainTimeout=0 时整段
	// 禁用，此时注册钩子由 Run() 返回 ErrDrainHooksRequireDrainTimeout。
	OnDrain(fns ...HookFunc)
	// OnPreStop 注册应用停止阶段执行的钩子函数：先于服务 Stop 执行
	// （此时服务仍在服务在途请求，适合"停止接收前的最后冲刷"，不适合
	// 释放在途请求还要用的底层资源——那类资源放 OnPostStop）。
	OnPreStop(fns ...HookFunc)
	// OnPostStart 注册服务启动后执行的"运行通知"钩子：所有服务 actor
	// 进入执行体（Start 调用紧随其后，可能与钩子的最初几条指令并发）、
	// run group 进入运行态后顺序执行。阻塞型服务（HTTP/gRPC server）
	// 的 Start 直到关停才返回，故不存在"所有 Start 已返回"的时刻——
	// 钩子触发不代表端口已监听或就绪（就绪语义用健康检查表达）。钩子
	// 错误视为启动失败，触发整个应用的关停并随 Run() 上抛。
	OnPostStart(fns ...HookFunc)
	// OnPostStop 注册进程收尾清理钩子：所有服务 Stop、总线关停之后、
	// Run 返回前逆序执行（LIFO，与 defer/Wire cleanup 语义一致），总预算
	// CleanupTimeout（默认 10 秒），超时记日志并跳过剩余钩子，不阻塞
	// 进程退出。用于释放 DI 底层资源（DB/Redis 连接池等）。签名是无
	// 错误返回的 CleanupFunc：终局阶段错误没有消费者。Run 的所有退出
	// 路径（含服务 Init 失败、PreStart 失败）都会执行；Run 从未调用时
	// 由 Close() 兜底。
	OnPostStop(fns ...CleanupFunc)
	// Register 注册需要由应用托管生命周期的服务实例。
	// 服务的 Init 在注册时同步执行；注册阶段产生的错误不会立即返回，
	// 首个错误会被记录，并在 Run() 时统一返回。
	// 所有注册必须先于 Run：Run 开始后调用将 panic（见 Run）。
	Register(services ...Service)
	// RegisterFactories 注册需要由应用托管生命周期的服务工厂，
	// 错误处理语义与 Register 相同；同样必须先于 Run。
	RegisterFactories(factories ...ServiceFactory)

	// Run 运行应用主流程：执行 on-pre-start 钩子、启动所有服务并等待退出信号。
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
	mu sync.Mutex
	o  *Options
	f  *pflag.FlagSet
	c  *viper.Viper
	// cfg 是权威的配置读取接口：默认路径在 initConfigure 完成后由 c 包装
	// 而成；WithConfig 注入时直接采用注入实例（跳过 flags/文件装配）。
	cfg            Config
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

	onPreStarts []HookFunc
	// onDrains 是排水钩子：SetDraining 之后与 DrainTimeout 睡眠并发执行，
	// 共享排水窗口预算（见 runOnDrainHooks）。
	onDrains     []HookFunc
	onPreStops   []HookFunc
	onPostStarts []HookFunc
	onPostStops  []CleanupFunc
	// startWG 计数已注册的服务：每个服务 actor 的执行体进入时 Done。
	// post-start actor 等它归零后才执行 OnPostStart hooks——触发界是
	// "所有服务 actor 已进入执行体"（Add 与 runG.Add 同持 app.mu 的登记
	// 事务，被 running 检查拒绝的服务不会计入，wg 不会悬挂）。
	startWG sync.WaitGroup
	// drain 是框架内部的排水检查器（见 drain.go）：DrainTimeout > 0 时由
	// newLynx 注册进 healthCheckers，关停时置位让 readiness 立即失败。
	// 手构的 lynx 实例（如测试辅助）可能为 nil，shutdown 路径需判空。
	drain *drainChecker
	// bus 是应用级消息总线（一等对象），始终可用（默认内存实现）。
	bus eventbus.Bus
	// busCancel 取消总线 Start 的独立上下文；Run 收尾或 Close 时调用。
	// Bus 在 newLynx 中提前 Start，故与 app.cancelCtx 分离。
	busCancel context.CancelFunc
	// initErr 记录注册阶段产生的首个错误，由 Run() 统一返回。
	initErr error
	// shutdownErrors 聚合服务 Stop 返回的错误与超时错误，由 Run() 统一上抛。
	shutdownErrors ShutdownErrors
}

func (app *lynx) OnPreStart(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onPreStarts = append(app.onPreStarts, fns...)
}

func (app *lynx) OnDrain(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onDrains = append(app.onDrains, fns...)
}

func (app *lynx) OnPreStop(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onPreStops = append(app.onPreStops, fns...)
}

func (app *lynx) OnPostStart(fns ...HookFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onPostStarts = append(app.onPostStarts, fns...)
}

func (app *lynx) OnPostStop(fns ...CleanupFunc) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onPostStops = append(app.onPostStops, fns...)
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
	// OnPreStart 等需要 app.mu 的方法时不会死锁。
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
// 一致（全局副作用见 App 接口注释；WithIsolated 可关闭该副作用）。
func (app *lynx) SetLogger(logger *slog.Logger) {
	if !app.o.isolated {
		slog.SetDefault(logger)
	}
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
	if app.running.Load() {
		// Run 已启动：post-stop 钩子与总线由 Run 的 defer 持有执行权，
		// 此处不抢先（runPostStopHooks 取走即清空，即使随后 Run 收尾
		// 调用也不会重复执行）。
		return
	}
	// Run 未启动（setup 失败/短路径）：清理提前 Start 的总线避免泄漏，
	// 并兜底执行 post-stop 钩子——它们没有其他执行机会。顺序与 Run
	// 收尾一致：总线先停，钩子最后。
	if app.busCancel != nil {
		_ = app.bus.Stop(context.Background())
		app.busCancel()
		app.busCancel = nil
	}
	app.runPostStopHooks()
}

func (app *lynx) init() error {
	if app.cfg == nil {
		if err := app.initConfigure(); err != nil {
			return err
		}
		app.cfg = NewViperConfig(app.c)
	}

	meta := Metadata{
		Name:    app.cfg.GetString("service.name"),
		ID:      app.cfg.GetString("service.id"),
		Version: app.cfg.GetString("service.version"),
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
// 执行时机在构建回调（SetupFunc）之前：此处 app.logger 尚未经过用户
// SetLogger 定制，重建 TextHandler 不会丢弃用户已设置的 handler 形态——
// 构建回调中的 SetLogger 在其后运行，其结果最终生效（用户优先）。
// 代价是若用户依赖 slog.SetDefault 预设非 TextHandler，此处会覆盖回
// TextHandler，需在回调内再次 SetLogger。
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
	// WithIsolated 下不触碰进程全局默认 logger。
	if !app.o.isolated {
		slog.SetDefault(app.logger)
	}
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
	if app.cfg != nil {
		return app.cfg
	}
	// 手构实例（如测试辅助）可能未经 init：保持既有行为，按需包装内部 viper。
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
		// OnPreStop hooks 先于服务 Stop 执行。
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
		app.startWG.Add(1)
		app.services = append(app.services, service)
		app.runG.Add(func() error {
			// 进入执行体即计数归零：post-start actor 以"所有服务 actor
			// 已进入执行体"为触发界（阻塞型服务的 Start 关停前不返回，
			// 不存在"所有 Start 已返回"的时刻）。
			app.startWG.Done()
			app.logger.InfoContext(ctx, "starting service", "service", service.Name())
			app.publishEvent(eventbus.TopicServiceStarting, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			// Started 事件契约：语义是"已进入运行"，不是"Start 已成功返回"。
			// 发布点固定在 Start 调用之前——阻塞式服务（如 HTTP/gRPC server）
			// 的 Start 只在关停时才返回，若等返回后再发布，订阅者整个生命周期
			// 都收不到 Started。代价：快速返回型服务若 Start 立即失败，订阅者
			// 会看到 Starting→Started→Failed 的时序，Failed 才是权威裁决，
			// 订阅侧不得仅凭 Started 认定服务持续可用（API 冻结，行为不变）。
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
			// 统一发布 Stopped：Stop 错误无法精确归属到单个服务（stopServiceBounded
			// 聚合进 shutdownErrors 由 Run 上抛），订阅侧以 Run 返回值/日志为准。
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

// stopServices 逆序停止已注册服务，用于 Init/OnPreStart 失败路径的资源清理。
func (app *lynx) stopServices(ctx context.Context) {
	app.mu.Lock()
	svcs := append([]Service(nil), app.services...)
	app.mu.Unlock()
	for i := len(svcs) - 1; i >= 0; i-- {
		app.stopServiceBounded(ctx, svcs[i])
	}
}

func (app *lynx) Run() error {
	// post-stop 收尾最先注册（defer LIFO 最后执行）：晚于本函数内注册的
	// 总线关停 defer，即"所有服务已 Stop、总线已停"之后才执行收尾钩子。
	// 注册在 initErr/OnPreStart 失败的早退路径之前，任何退出路径都会
	// 释放已注册的清理钩子。runPostStopHooks 取走即清空，Close() 的
	// 兜底调用不会重复执行。
	defer app.runPostStopHooks()
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

	// 排水配置快失败：v1.10.0 起排水窗口即 OnDrain 钩子的总预算
	//（DrainHookTimeout 已并入 DrainTimeout），窗口未启用（0）时钩子
	// 没有执行预算——注册即配置错误。启动期返回（镜像 initErr 的
	// poison-pill 语义：已 Init 的服务逆序停止），好过关停期静默跳过
	// 注销钩子的延迟暴露。
	if app.o.DrainTimeout <= 0 && app.hasDrainHooks() {
		app.stopServices(app.ctx)
		app.publishEvent(eventbus.TopicAppStopped, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
		return ErrDrainHooksRequireDrainTimeout
	}

	// 退出信号提前注册：OnPreStart hook 阻塞期间收到的信号进入缓冲 chan，
	// hook 结束后立即触发关停——此前信号注册在 hook 之后，阻塞的 hook
	// 会使进程对 SIGTERM/SIGINT 无响应。
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh, app.o.ExitSignals...)
	defer signal.Stop(exitCh)

	// 顺序执行 OnPreStart hooks，全部成功后服务才开始启动。
	if err := app.runOnPreStartHooks(); err != nil {
		// 未进入 run.Group：已 Init 的服务需手动逆序清理，释放资源。
		app.stopServices(app.ctx)
		app.publishEvent(eventbus.TopicAppStopped, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
		return err
	}
	app.publishEvent(eventbus.TopicAppStarted, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})

	// 总线已在 newLynx 提前 Start；此处仅托管 last-actor 关停，确保
	// AppStopped 等收尾事件能在 Bus.Stop 前投递。
	defer func() {
		app.publishEvent(eventbus.TopicServiceStopping, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
		busCtx := context.WithoutCancel(app.ctx)
		app.stopServiceBounded(busCtx, busService{app.bus})
		app.publishEvent(eventbus.TopicServiceStopped, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
		if app.busCancel != nil {
			app.busCancel()
		}
	}()

	// 关闭 actor：收到退出信号或应用上下文被取消时，先在 actor 内执行关停
	// 流程：置位 drainChecker 后与排水窗口睡眠并发执行 OnDrain hooks（如
	// 从服务发现注销，窗口即钩子总预算），随后执行 OnPreStop hooks，返回后
	// run.Group 才按注册顺序停止服务——保证清理逻辑发生在服务仍在服务期间。
	// OnDrain/OnPreStop 错误随 Run() 上抛，让调用方（如 K8s）感知关停失败。
	var (
		shutdownOnce sync.Once
		shutdownErr  error
		drainErr     error
	)
	shutdown := func() {
		app.Logger().Info("shutting down")
		app.publishEvent(eventbus.TopicAppStopping, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
		// Step 0: 排水窗口（可选）。置位 drainChecker 使 readiness 聚合立即
		// 失败（LB 摘流），等待 DrainTimeout 窗口结束后才执行后续关停。
		// 窗口同时是 OnDrain 钩子的总预算（v1.10.0 起合并，DrainHookTimeout
		// 已移除）：钩子与睡眠并发，窗口结束即继续——上界不叠加。
		// 所有关停入口（信号/中断/Close）都经过本函数，排水窗口统一生效。
		// DrainTimeout=0 时整段跳过（Run 入口已保证此路径不可能有已注册的
		// OnDrain 钩子），不增加任何等待。
		if app.drain != nil {
			app.drain.SetDraining(true)
		}
		if app.o.DrainTimeout > 0 {
			app.publishEvent(eventbus.TopicDrainStarting, eventbus.DrainEvent{Timeout: app.o.DrainTimeout, Time: time.Now()})
			drainHooksDone := make(chan struct{})
			if app.hasDrainHooks() {
				// 窗口 deadline 即钩子总预算：在窗口开启时创建并传入，
				// 钩子预算与窗口严格对齐（无论钩子 goroutine 调度迟早）。
				drainCtx, drainCancel := context.WithTimeout(context.Background(), app.o.DrainTimeout)
				defer drainCancel()
				go func() {
					defer close(drainHooksDone)
					drainErr = app.runOnDrainHooks(drainCtx)
				}()
			} else {
				close(drainHooksDone)
			}
			app.Logger().Info("draining: readiness marked unhealthy, waiting for drain window",
				"drain_timeout", app.o.DrainTimeout.String())
			// 窗口不可被 ctx 取消打断：排水语义要求服务在窗口内保持运行，
			// 供在途请求收尾。
			time.Sleep(app.o.DrainTimeout)
			app.publishEvent(eventbus.TopicDrainCompleted, eventbus.DrainEvent{Timeout: app.o.DrainTimeout, Time: time.Now()})
			// 等待 OnDrain 钩子收尾（受窗口预算约束，不会挂死）。
			<-drainHooksDone
		}
		// Step 1: 取消应用上下文，通知服务开始收尾。
		app.cancelCtx()
		// Step 2: 在 ShutdownTimeout 内执行 OnPreStop hooks。
		shutdownErr = app.runOnPreStopHooks()
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
	// post-start actor：startWG 归零（所有服务 actor 已进入执行体）后顺序
	// 执行 OnPostStart hooks。钩子错误从 actor 返回，run.Group 中断其余
	// actor 走既有关停路径——与"服务 Start 失败"同一语义。
	// postStartCtx 刻意不挂接 app.ctx：若监听应用上下文取消，Close 时
	// 本 actor 会先于 signal actor 返回，成为 run.Group 的首个返回者并
	// 抢跑中断序列，把 OnPreStop 挤到服务 Stop 之后（破坏关停不变量）。因此与服务一致：仅由自身的 interrupt 取消——正常
	// 关停路径中 signal actor 完成 shutdown（含 OnPreStop）并返回后才
	// 中断到本 actor。interrupt / 应用上下文取消视为"被关停打断"：
	// 未执行完的钩子不再等待（其 goroutine 仍在后台运行，与
	// stopServiceBounded 相同的取舍），不向已进入关停的进程注入新错误。
	postStartCtx, postStartCancel := context.WithCancel(context.WithoutCancel(app.ctx))
	app.runG.Add(func() error {
		app.startWG.Wait()
		return app.runOnPostStartHooks(postStartCtx)
	}, func(err error) {
		postStartCancel()
	})
	app.mu.Unlock()

	// Step 3: run.Group 在第一个 actor 返回后停止所有服务。
	// 服务 Start 先失败时 oklog/run 只返回首个 actor 错误；此处把 run group
	// 错误、OnDrain/OnPreStop 钩子错误与服务 Stop 错误聚合后一并上抛（nil 安全）。
	runErr := app.runG.Run()
	app.publishEvent(eventbus.TopicAppStopped, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
	if app.shutdownErrors.HasErrors() {
		return errors.Join(runErr, shutdownErr, drainErr, &app.shutdownErrors)
	}
	return errors.Join(runErr, shutdownErr, drainErr)
}

func (app *lynx) runOnPreStartHooks() error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onPreStarts...)
	app.mu.Unlock()

	app.Logger().Info("run on-pre-start hooks")
	for _, fn := range hooks {
		if err := fn(app.ctx); err != nil {
			return err
		}
	}
	return nil
}

// runOnPostStartHooks 顺序执行 OnPostStart hooks：startWG 已保证触发时
// 所有服务 actor 均已进入执行体（运行态）。钩子在独立 goroutine 中执行
// 并监听 ctx 取消——被关停打断时不再等待（避免挂起 Run 收尾），视为打断
// 记日志返回 nil；钩子自身错误返回并触发整个应用的关停（启动失败语义）。
// ctx 已取消后到达的钩子错误同样按打断处理，不注入关停中的进程。
// 钩子全部完成后阻塞等待 ctx（run.Group actor 契约：execute 必须阻塞
// 到完成，立即返回会被 run.Group 视为首个完成者而触发关停）。
func (app *lynx) runOnPostStartHooks(ctx context.Context) error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onPostStarts...)
	app.mu.Unlock()

	if len(hooks) == 0 {
		<-ctx.Done()
		return nil
	}
	app.Logger().Info("run on-post-start hooks")
	for _, fn := range hooks {
		done := make(chan error, 1)
		go func() { done <- fn(ctx) }()
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				return err
			}
		case <-ctx.Done():
			app.logger.WarnContext(app.ctx, "on-post-start hooks interrupted by shutdown")
			return nil
		}
	}
	<-ctx.Done()
	return nil
}

// runPostStopHooks 逆序执行 OnPostStop hooks：进程收尾清理（关闭
// DB/Redis 连接池等）。取走即清空——Close() 的兜底路径与 Run 的 defer
// 路径不会重复执行。总预算 CleanupTimeout：单个钩子挂起时记日志跳过
// 剩余钩子（其 goroutine 仍在后台运行，与 stopServiceBounded 相同的
// 取舍——不阻塞进程退出优先）。钩子签名为 CleanupFunc（无错误返回）：
// 终局阶段错误没有消费者，实现方自行记日志。
func (app *lynx) runPostStopHooks() {
	app.mu.Lock()
	fns := app.onPostStops
	app.onPostStops = nil
	app.mu.Unlock()
	if len(fns) == 0 {
		return
	}
	app.Logger().Info("run on-post-stop hooks")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), app.o.CleanupTimeout)
	defer cancel()
loop:
	for i := len(fns) - 1; i >= 0; i-- {
		done := make(chan struct{})
		go func(fn CleanupFunc) {
			fn()
			close(done)
		}(fns[i])
		select {
		case <-done:
		case <-ctx.Done():
			// 倒序执行：fns[i] 超时，尚未执行的 fns[0..i-1] 共 i 个被跳过。
			app.logger.ErrorContext(app.ctx, "on-post-stop hook did not complete within cleanup timeout",
				"timeout", app.o.CleanupTimeout.String(), "skipped", i)
			break loop
		}
	}
	app.Logger().Info("on-post-stop hooks finished", "elapsed", time.Since(start).Round(time.Millisecond).String())
}

// hasDrainHooks 报告是否注册了 OnDrain 钩子。无钩子时关停路径整段跳过
// 钩子执行，不增加任何等待（回归红线：默认关停上界与既有版本一致）。
func (app *lynx) hasDrainHooks() bool {
	app.mu.Lock()
	defer app.mu.Unlock()
	return len(app.onDrains) > 0
}

// runOnDrainHooks 在排水窗口预算内顺序执行所有 OnDrain hooks（ctx 由
// shutdown 闭包在窗口开启时创建，deadline 即窗口结束）。语义对齐
// runOnPreStopHooks：单个 hook 阻塞不会挂起整个关闭流程，超时记录错误
// 并继续；钩子错误不打断排水。不传没有 deadline 的 app.ctx。
func (app *lynx) runOnDrainHooks(ctx context.Context) error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onDrains...)
	app.mu.Unlock()

	app.Logger().Info("run on-drain hooks")

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

// runOnPreStopHooks 在 ShutdownTimeout 内顺序执行所有 OnPreStop hooks。
// 单个 hook 阻塞不会挂起整个关闭流程：超过时限后记录错误并继续。
// 收集到的错误（含超时）以 *ShutdownErrors 返回，由 Run() 上抛给调用方。
func (app *lynx) runOnPreStopHooks() error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onPreStops...)
	app.mu.Unlock()

	app.Logger().Info("run on-pre-stop hooks")
	ctx, cancel := context.WithTimeout(context.Background(), app.o.ShutdownTimeout)
	defer cancel()

	var shutdownErrors ShutdownErrors
	for _, fn := range hooks {
		if ctx.Err() != nil {
			shutdownErrors.Add(errors.New("shutdown timeout exceeded while running on-pre-stop hooks"))
			break
		}
		done := make(chan error, 1)
		go func() { done <- fn(ctx) }()
		select {
		case hookErr := <-done:
			if hookErr != nil {
				app.logger.ErrorContext(app.ctx, "on-pre-stop hook called error", "error", hookErr)
				shutdownErrors.Add(hookErr)
			}
		case <-ctx.Done():
			app.logger.ErrorContext(app.ctx, "on-pre-stop hook did not complete within shutdown timeout")
			shutdownErrors.Add(errors.New("on-pre-stop hook timed out"))
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

func (s busService) Name() string                    { return s.b.Name() }
func (s busService) Init(ctx AppContext) error       { return s.b.Init(ctx) }
func (s busService) Start(ctx context.Context) error { return s.b.Start(ctx) }
func (s busService) Stop(ctx context.Context) error  { return s.b.Stop(ctx) }

// NewApp 按选项构造 Lynx 应用并返回 App 实例，不经 Runner 包装：
// 适用于测试（配合 lynxtest）与把 Lynx 嵌入宿主进程的场景。构造语义与
// NewRunner 严格一致（opts 应用在空 Options 上，再由 newLynx 做
// EnsureDefaults/Validate、配置装配、总线提前 Start 与就绪等待——顺序
// 敏感的 Option 如 WithBusOptions 依赖 Bus 尚未填充默认值的空 Options），
// 初始化错误经返回值给出。调用方随后 Register 组装服务并 Run；Run 从未
// 启动时以 Close 释放（停止总线并兜底 post-stop 钩子）。
func NewApp(opts ...Option) (App, error) {
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	return newLynx(o)
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
		o:            o,
		c:            viper.New(),
		cfg:          o.Config,
		f:            f,
		runG:         &run.Group{},
		logger:       slog.Default(),
		onPreStarts:  []HookFunc{},
		onDrains:     []HookFunc{},
		onPreStops:   []HookFunc{},
		onPostStarts: []HookFunc{},
		onPostStops:  []CleanupFunc{},
		bus:          o.Bus,
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
	// 总线单独初始化并提前 Start（不经 addServices 健康聚合）：
	// Component Init 中即可 Subscribe（Watermill: AddConsumerHandler+RunHandlers）并收到 Publish。
	// Start/Stop 以 last-actor 语义在 Run 收尾托管。
	if err := app.bus.Init(app); err != nil {
		return nil, err
	}
	busCtx, busCancel := context.WithCancel(context.WithoutCancel(app.ctx))
	app.busCancel = busCancel
	app.publishEvent(eventbus.TopicServiceStarting, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
	go func() {
		if err := app.bus.Start(busCtx); err != nil {
			app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now(), Error: err.Error()})
		}
	}()
	// 就绪等待受 BusReadyTimeout 总预算约束（默认 10s，可经
	// WithBusReadyTimeout 配置）：此前硬编码 1 秒会让 Watermill+Kafka 等
	// 慢启动后端在正常部署下构造失败。轮询间隔保持 10ms 量级——后端
	// 就绪无通知机制，忙轮询是唯一手段；预算耗尽仍不健康则快失败，
	// 优于带病运行。
	readyDeadline := time.Now().Add(o.BusReadyTimeout)
	for time.Now().Before(readyDeadline) {
		if app.bus.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := app.bus.CheckHealth(); err != nil {
		busCancel()
		return nil, fmt.Errorf("lynx: bus failed to become ready within %s: %w", o.BusReadyTimeout, err)
	}
	app.publishEvent(eventbus.TopicServiceStarted, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
	// WithIsolated：跳过进程级全局注册，同进程多 App（测试/宿主内嵌）
	// 互不污染。ctx 内嵌总线保留——那是请求作用域而非进程全局。
	if !o.isolated {
		eventbus.SetDefault(app.bus)
	}
	app.ctx = eventbus.ContextWithBus(app.ctx, app.bus)
	if !o.isolated {
		Set(app)
	}
	return app, nil
}
