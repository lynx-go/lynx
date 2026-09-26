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
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lynx-go/lynx/eventbus"
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
	// Command 注册启动的命令，用于 CLI 模式。选项经 opts 传入
	// （WithMaxTries / WithBackoff / WithProbeTimeout / WithCommandName）。
	Command(cmd CommandFunc, opts ...CommandOption) error

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
	// 释放在途请求还要用的底层资源——那类资源放 OnPostStop）。服务进入
	// 运行阶段后，所有退出路径（信号/Close/服务失败/钩子失败/命令完成）
	// 都保证此顺序；Init/OnPreStart 失败路径不执行本阶段。
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
	// 所有注册必须先于 Run 与 Close：Run 开始后或 Close 之后调用将 panic。
	Register(services ...Service)
	// RegisterFactory 注册需要由应用托管生命周期的服务工厂，
	// 错误处理语义与 Register 相同；同样必须先于 Run 与 Close。
	RegisterFactory(factories ...ServiceFactory)

	// Run 运行应用主流程：执行 on-pre-start 钩子、启动所有服务并等待退出信号。
	// Run 开始后或 Close 之后，Register/RegisterFactory 为禁止操作（panic），
	// Command 返回错误。
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

// ContextWithMeta 返回携带应用元数据的 ctx（Meta 的对称写入口）：框架在
// init 时经此写入；lynxtest 与宿主嵌入场景可用同一入口构造带元数据的
// 上下文，使 lynx.Meta 可见。
func ContextWithMeta(parent context.Context, meta Metadata) context.Context {
	return context.WithValue(parent, keyMeta, meta)
}

type lynx struct {
	mu sync.Mutex
	o  *Options
	f  *pflag.FlagSet
	c  *viper.Viper
	// cfg 是权威的配置读取接口：默认路径在 initConfigure 完成后由 c 包装
	// 而成；WithConfig 注入时直接采用注入实例（跳过 flags/文件装配）。
	cfg Config
	ctx context.Context
	// logLevelVar 非 nil（applyLogLevel 自建 handler 路径）时为运行时
	// 可调的级别变量；loggerCustom 标记用户 SetLogger 定制过 handler
	// （此后框架不再代理级别调整）；lastLevel 是级别记账值（未显式
	// 设置时为 Info），供 LogLevel 查询。
	logLevelVar    *slog.LevelVar
	loggerCustom   bool
	lastLevel      slog.Level
	cancelCtx      context.CancelFunc
	logger         *slog.Logger
	healthCheckers []Checker
	// services 按注册顺序记录已 Init 成功的服务，用于失败路径的逆序清理。
	services []Service
	// servicesStopped 标记批次停止已执行（受 app.mu 保护）：Init 失败
	// （addServices）、Run 早退与 Close 兜底可能先后触发停止，服务 Stop
	// 不得被重复调用（Lifecycle 契约只保证容忍 Stop 先于 Start，不保证幂等）。
	servicesStopped bool
	// actors 是按注册顺序登记的服务执行单元（execute/stop），由 lifecycle
	// 在 Run 时快照并调度；关停时逆序停止（LIFO）。
	actors []actor
	// running 标记 Run 已开始（受 app.mu 保护）：此后 Register/
	// RegisterFactory 为禁止操作（panic），Command 返回错误；Run 的
	// actor 快照在置位后取得，不存在并发登记。
	running bool
	// closed 标记 Close 已执行（受 app.mu 保护）：Close 之后的 Run 直接
	// 返回 ErrAppClosed，不执行任何钩子与服务；Close 之后的注册同样被
	// 拒绝（panic / ErrAppClosed），不产生孤儿服务。
	closed bool

	onPreStarts []HookFunc
	// onDrains 是排水钩子：SetDraining 之后与 DrainTimeout 睡眠并发执行，
	// 共享排水窗口预算（见 runOnDrainHooks）。
	onDrains     []HookFunc
	onPreStops   []HookFunc
	onPostStarts []HookFunc
	onPostStops  []CleanupFunc
	// startWG 计数已注册的服务：每个服务 actor 的执行体进入时 Done。
	// post-start actor 等它归零后才执行 OnPostStart hooks——触发界是
	// "所有服务 actor 已进入执行体"（registerService 的登记事务与
	// startWG.Add 同持 app.mu，被拒绝的服务不会计入，wg 不会悬挂）。
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
	if initErr, err := app.checkRegistration(); err != nil {
		// 所有注册必须先于 Run 与 Close。无返回值的注册入口以 panic
		// 明确报错（编程错误）。
		panic(registrationError("Register", err))
	} else if initErr != nil {
		return
	}
	// addServices 在锁外执行 Init：服务 Init 内调用 app.HealthCheckers()、
	// OnPreStart 等需要 app.mu 的方法时不会死锁。
	if err := app.addServices(services...); err != nil {
		if errors.Is(err, errRunStarted) || errors.Is(err, ErrAppClosed) {
			// 与 Run/Close 并发的迟到注册：持锁登记事务内的权威裁决点。
			panic(registrationError("Register", err))
		}
		app.recordInitError(err)
		app.logger.ErrorContext(app.ctx, "failed to register services", "error", err)
	}
}

func (app *lynx) RegisterFactory(factories ...ServiceFactory) {
	if initErr, err := app.checkRegistration(); err != nil {
		panic(registrationError("RegisterFactory", err))
	} else if initErr != nil {
		return
	}
	if err := app.addServiceFactories(factories...); err != nil {
		if errors.Is(err, errRunStarted) || errors.Is(err, ErrAppClosed) {
			panic(registrationError("RegisterFactory", err))
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
// 调用方（Register/RegisterFactory/Command）翻译为各自的明确错误/panic
// （所有注册必须先于 Run）。
var errRunStarted = errors.New("lynx: registration after Run() has started")

// SetLogger 设置 logger，并同步 slog.SetDefault 使全局默认 logger 与应用
// 一致（全局副作用见 App 接口注释；WithIsolated 可关闭该副作用）。
// 设置后视为用户定制：SetLogLevel 不再代理该 logger 的级别调整。
func (app *lynx) SetLogger(logger *slog.Logger) {
	if !app.o.isolated {
		slog.SetDefault(logger)
	}
	app.loggerCustom = true
	app.logger = logger
}

// SetLogLevel 运行时调整应用默认 logger 的日志级别（运行时可调性，
// debug 服务的 /loglevel 端点经此生效）。返回 false 表示当前 logger
// 不可由框架调整（用户 SetLogger 定制过 handler 形态）。两条生效路径：
//   - 配置过 log-level（applyLogLevel 自建 LevelVar handler）：直接修改
//     级别变量，app.logger 与 slog.Default()（同源）即时生效；
//   - 未配置（logger 为 slog.Default()）：经 slog.SetLogLoggerLevel，
//     仅影响内置 default handler（zap 等自建 handler 的 contrib 不受
//     影响，级别调整走各自机制）。
func (app *lynx) SetLogLevel(level slog.Level) bool {
	if app.loggerCustom {
		return false
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.logLevelVar != nil {
		app.logLevelVar.Set(level)
	} else {
		slog.SetLogLoggerLevel(level)
	}
	app.lastLevel = level
	return true
}

// LogLevel 返回日志级别的记账值：显式设置（配置 log-level 或
// SetLogLevel）后为最后设置值，否则为 Info。用户 SetLogger 定制路径的
// handler 级别不可见，同样返回记账值。
func (app *lynx) LogLevel() slog.Level {
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.lastLevel
}

// HealthCheckers 返回当前已注册的健康检查器快照。
func (app *lynx) HealthCheckers() []Checker {
	app.mu.Lock()
	defer app.mu.Unlock()
	out := make([]Checker, len(app.healthCheckers))
	copy(out, app.healthCheckers)
	return out
}

func (app *lynx) Command(cmd CommandFunc, opts ...CommandOption) error {
	if initErr, err := app.checkRegistration(); err != nil {
		return registrationError("Command", err)
	} else if initErr != nil {
		return initErr
	}
	if err := app.addServices(NewCommand(cmd, opts...)); err != nil {
		if errors.Is(err, errRunStarted) || errors.Is(err, ErrAppClosed) {
			return registrationError("Command", err)
		}
		app.recordInitError(err)
		return err
	}
	return nil
}

func (app *lynx) Close() {
	app.mu.Lock()
	if app.closed {
		// 幂等：二次 Close 无操作。
		app.mu.Unlock()
		return
	}
	// closed 与 Run 入口的 running 置位在同一互斥域内完成置位/检查：
	// Close 与尚未调度的 Run 竞争时只有一个语义胜出——要么 Run 入口
	// 看到 closed 返回 ErrAppClosed，要么本函数看到 running 走既有路径。
	app.closed = true
	running := app.running
	app.mu.Unlock()
	if running {
		// Run 已启动：关停阶段序列由 runLifecycle 持有执行权，此处只取消
		// 应用 Context 触发 app-context 关停分支，不抢先（post-stop 钩子
		// 取走即清空，Run 收尾不会重复执行）。
		app.cancelCtx()
		return
	}
	// Run 未启动（setup 失败/短路径）：走关停快路径释放已 Init 的服务与
	// 提前 Start 的总线，并兜底执行 post-stop 钩子——它们没有其他执行
	// 机会。顺序与 Run 早退一致：停服务（Stop 收到仍存活的 ctx）→ 停总线
	// → 取消应用 Context → 收尾钩子。Close 无返回值，停止错误进入
	// shutdownErrors 并由 stopServiceBounded 记 Error 日志。
	t := teardown{app}
	t.stopServices(app.ctx)
	t.stopBus()
	t.cancelCtx()
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
	app.ctx = ContextWithMeta(app.ctx, meta)

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
	app.logLevelVar = &levelVar
	app.lastLevel = level
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

// serviceSnapshot 返回已注册服务的快照（command 的依赖等待按三级
// 优先逐服务解析就绪信号）。注册先于 Run 的契约保证快照集合在
// Run 期间不变，取一次即稳定。
func (app *lynx) serviceSnapshot() []Service {
	app.mu.Lock()
	defer app.mu.Unlock()
	return append([]Service(nil), app.services...)
}

// publishAppEvent 发布应用级生命周期事件（AppStarting/AppStarted/
// AppStopping/AppStopped），payload 携带当前应用元数据。
func (app *lynx) publishAppEvent(topic string) {
	meta := Meta(app.ctx)
	app.publishEvent(topic, eventbus.AppEvent{Name: meta.Name, ID: meta.ID, Version: meta.Version, Time: time.Now()})
}

// publishEvent 发布内建生命周期事件，失败仅记 debug 日志，不影响主流程。
// startConfigWatch 注册配置文件热更新（WithConfigWatch）：viper WatchConfig
// 在文件变更时已自动重读（后续 Get 返回新值），此处仅桥接事件——发布
// lynx.config.updated 到总线（lynx.* 前缀在 watermill 等跨进程 Bus 上
// 强制内存路由，热更新事件不出进程）。已知取舍：viper 的 watcher 无
// 撤销 API，watch 随进程常驻（与 AUX-14 全局 provider 不复位同类）。
// 配置非文件来源（WithConfig 注入/无 --config）时报错，由调用方以
// poison-pill 语义处理。
func (app *lynx) startConfigWatch() error {
	if app.c == nil || app.c.ConfigFileUsed() == "" {
		return errors.New("lynx: WithConfigWatch requires a config file (--config / -c or WithConfigFile)")
	}
	app.c.OnConfigChange(func(in fsnotify.Event) {
		app.publishEvent(eventbus.TopicConfigUpdated, eventbus.ConfigUpdatedEvent{
			File: in.Name,
			Time: time.Now(),
		})
	})
	app.c.WatchConfig()
	app.logger.Info("config watch started", "file", app.c.ConfigFileUsed())
	return nil
}

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
		// 服务由自己的 actor stop（Stop + cancel）停止，从而保证关闭时
		// OnPreStop hooks 先于服务 Stop 执行。
		ctx, cancel := context.WithCancel(context.WithoutCancel(app.ctx))
		app.logger.InfoContext(ctx, "initializing service", "service", service.Name())
		// Init 在锁外执行（调用方不持 app.mu）：Init 内调用
		// app.HealthCheckers() 等需要 app.mu 的方法时不会死锁。
		if err := service.Init(app); err != nil {
			cancel()
			app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now(), Error: err.Error()})
			// 逆序有界停止本批及此前已 Init 成功的服务，释放其打开的资源。
			// 批次幂等：Run 早退/Close 的快路径不会重复停止。
			app.stopServices(app.ctx)
			return err
		}
		app.logger.InfoContext(ctx, "initialized service", "service", service.Name())
		app.publishEvent(eventbus.TopicServiceRegistered, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
		// 登记事务（lifecycle.registerService）：running/closed 检查与
		// actor 登记同持 app.mu。这是与 Run/Close 并发时的权威裁决点——
		// 任何迟到的登记都不可能越过检查；检查失败时服务不进入
		// actors/services/healthCheckers（无孤儿）。
		if err := app.registerService(service, app.serviceActor(ctx, cancel, service)); err != nil {
			cancel()
			return err
		}
	}
	return nil
}

// stopServiceBounded / stopServices / 排水与各阶段钩子执行器已收敛至
// shutdown.go；actor 调度、阶段序列与注册状态机收敛至 lifecycle.go
//（两者的唯一归属）。

func (app *lynx) Run() error {
	// post-stop 收尾最先注册（defer LIFO 最后执行）：晚于 runLifecycle 的
	// 关停阶段序列，即"所有服务已 Stop、总线已停"之后才执行收尾钩子。
	// 注册在 initErr/OnPreStart 失败的早退路径之前，任何退出路径都会
	// 释放已注册的清理钩子。runPostStopHooks 取走即清空，Close() 的
	// 兜底调用不会重复执行。
	defer app.runPostStopHooks()
	app.mu.Lock()
	if app.closed {
		// Close 已先行（Run goroutine 尚未被调度时被释放）：不再执行任何
		// 钩子与服务。post-stop 钩子已由 Close 清空，此处 defer 是空转。
		app.mu.Unlock()
		return ErrAppClosed
	}
	initErr := app.initErr
	// running 在持 app.mu 时置位——与注册侧持锁登记事务的 running 检查
	// 互斥（lifecycle.registerService）；同时作为 Run 的单次守卫：二次
	// 调用直接返回错误，服务不会被二次 Start/Stop。
	alreadyRunning := app.running
	app.running = true
	app.mu.Unlock()
	if initErr != nil {
		// 启动期 poison-pill：不进入运行阶段，走关停快路径释放已 Init 的
		// 服务与提前 Start 的总线（Run 已置位 running，Close 不再兜底），
		// 返回触发错误与关停错误的聚合。
		return teardown{app}.fast(initErr)
	}
	if alreadyRunning {
		return errors.New("lynx: Run must not be called more than once")
	}
	app.Logger().Info("starting")
	app.publishAppEvent(eventbus.TopicAppStarting)

	// 排水配置快失败：v1.10.0 起排水窗口即 OnDrain 钩子的总预算
	//（DrainHookTimeout 已并入 DrainTimeout），窗口未启用（0）时钩子
	// 没有执行预算——注册即配置错误。启动期返回（镜像 initErr 的
	// poison-pill 语义：已 Init 的服务逆序停止），好过关停期静默跳过
	// 注销钩子的延迟暴露。
	if app.o.DrainTimeout <= 0 && app.hasDrainHooks() {
		return teardown{app}.fast(ErrDrainHooksRequireDrainTimeout)
	}

	// 配置热更新（WithConfigWatch）：注册失败镜像 initErr 的 poison-pill
	// 语义（已 Init 的服务逆序停止后返回），显式要求热更新却无从 watch
	// 的配置错误在启动期暴露。
	if app.o.ConfigWatch {
		if err := app.startConfigWatch(); err != nil {
			return teardown{app}.fast(err)
		}
	}

	// 退出信号提前注册：OnPreStart hook 阻塞期间收到的信号进入缓冲 chan，
	// hook 结束后立即触发关停——此前信号注册在 hook 之后，阻塞的 hook
	// 会使进程对 SIGTERM/SIGINT 无响应。
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh, app.o.ExitSignals...)
	defer signal.Stop(exitCh)

	// 顺序执行 OnPreStart hooks，全部成功后服务才开始启动。
	if err := app.runOnPreStartHooks(); err != nil {
		// 未进入 actor 调度：走关停快路径逆序清理已 Init 的服务并停总线。
		// 关停阶段（drain/OnPreStop）只在服务进入运行阶段后执行（契约
		// 见 lifecycle.go 文件头与 docs/03-core-concepts.md）。
		return teardown{app}.fast(err)
	}
	app.publishAppEvent(eventbus.TopicAppStarted)

	// 进入 lifecycle 阶段机：actor 调度、触发裁决、关停阶段序列与错误
	// 聚合的唯一归属（见 lifecycle.go 文件头）。
	return app.runLifecycle(exitCh)
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
func (app *lynx) runOnPostStartHooks(ctx context.Context) error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onPostStarts...)
	app.mu.Unlock()

	if len(hooks) == 0 {
		return nil
	}
	app.Logger().Info("run on-post-start hooks")
	for _, fn := range hooks {
		hookErr, interrupted := callBounded(ctx, func() error { return fn(ctx) })
		if interrupted {
			app.logger.WarnContext(app.ctx, "on-post-start hooks interrupted by shutdown")
			return nil
		}
		if hookErr != nil && ctx.Err() == nil {
			return hookErr
		}
	}
	<-ctx.Done()
	return nil
}

// runPostStopHooks / hasDrainHooks / runOnDrainHooks / runOnPreStopHooks
// 已收敛至 shutdown.go。

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
	// WithBusProvider：依赖配置的总线（如 watermill NewFromConfig）在配置
	// 装配完成后、总线初始化前解析；仅当 WithBus 未显式注入时咨询（显式
	// 实例优先；WithBusOptions 物化的内存总线可被 provider 覆盖，顺序无关）。
	// 配套服务延后到总线就绪、ctx 内嵌完成之后再注册：注册期的服务生命
	// 周期事件（publishEvent 固定打 app.bus）应落在已启动的总线上，服务
	// ctx 也应携带最终总线。
	var busServices []Service
	if o.BusProvider != nil && (o.Bus == nil || o.busFromOptions) {
		bus, services, err := o.BusProvider(app.cfg)
		if err != nil {
			return nil, fmt.Errorf("lynx: bus provider failed: %w", err)
		}
		if bus == nil {
			return nil, errors.New("lynx: bus provider returned nil bus")
		}
		app.bus = bus
		busServices = services
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
	// busStartErr 捕获 Start 的根因：明确失败即刻返回，不退化成长达
	// BusReadyTimeout 的就绪超时；Start 正常返回（非阻塞实现）不入通道，
	// 就绪等待继续（与 probe.wait 的 startErr 交错语义一致：非 nil 错误
	// 立即返回，nil 值才表示「等待期间正常收尾」）。
	busStartErr := make(chan error, 1)
	go func() {
		if err := app.bus.Start(busCtx); err != nil {
			busStartErr <- fmt.Errorf("lynx: bus start failed: %w", err)
			app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now(), Error: err.Error()})
		}
	}()
	// 就绪等待受 BusReadyTimeout 总预算约束（默认 10s，可经
	// WithBusReadyTimeout 配置）：此前硬编码 1 秒会让 Watermill+Kafka 等
	// 慢启动后端在正常部署下构造失败。轮询与 startErr 交错统一经
	// ready.go 的 probe.wait（与 OrderedServices 同一路径）；预算耗尽仍
	// 不健康则快失败，优于带病运行；Start 明确失败时返回根因。
	waitCtx, waitCancel := context.WithCancel(context.Background())
	defer waitCancel()
	busReadyErr := checkerProbe(app.bus).wait(waitCtx, o.BusReadyTimeout, busStartErr,
		func(last error) error {
			return fmt.Errorf("lynx: bus failed to become ready within %s: %w", o.BusReadyTimeout, last)
		})
	if busReadyErr != nil {
		busCancel()
		return nil, busReadyErr
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
	// provider 配套服务最后注册（构造收尾）：Init 同步执行，Start/Stop 由
	// 生命周期托管，逆序 Stop 时最后停止（总线 Stop 在所有服务之后，即
	// Transport 先于 Bus 关闭）。注册失败与就绪超时同样以 busCancel 收尾。
	if len(busServices) > 0 {
		if err := app.addServices(busServices...); err != nil {
			busCancel()
			return nil, err
		}
	}
	return app, nil
}
