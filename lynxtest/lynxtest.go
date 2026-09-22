// Package lynxtest 提供基于 Lynx 的应用的测试辅助。
//
// 两类入口对应两层测试：
//
//   - Run：L2 组装测试——用与生产 main 相同的 Setup 函数在测试进程内拉起
//     完整应用（服务、hooks、总线、关停序列），环境差异全部经配置注入
//     （WithConfigFile/WithConfigYAML/WithConfigMap，低→高叠加），组装代码
//     不写测试分支。
//   - NewContext：L1 单元测试——提供可用的 lynx.AppContext（真内存总线 +
//     注入配置 + 接 testing.TB 的日志），替代手写 fake。
//
// 配置默认封闭：Run 总是注入内存配置，不解析 os.Args、不搜索工作目录，
// 测试二进制的参数与包目录里的 config.yaml 不会隐式生效；需要以生产配置
// 为基线时用 WithConfigFile 显式加载。
//
// 并行限制：Run 默认不隔离进程级全局（lynx.Set / eventbus.SetDefault /
// slog.SetDefault），用例结束后恢复先前值。因此同包内存在任何
// t.Parallel() 用例时，Run 管理的用例必须显式 WithOptions(lynx.WithIsolated())；
// 纯串行包无此约束。一个用例一个 App：Run 是单次语义，不可重启。
package lynxtest

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// 基线超时：全部压到 Options 允许的下限或小值。实际停止通常远快于上界，
// 这些值只约束最坏情况，避免测试挂死而非加速常路径。
// 注意：关闭真实资源（DB 连接池等）的 OnPostStop 钩子在 1s CleanupTimeout
// 下可能被截断，重服务用 WithOptions(lynx.WithCleanupTimeout(...)) 放宽。
const (
	baseTimeout  = 1 * time.Second
	baseDrain    = 100 * time.Millisecond
	runStopWait  = 30 * time.Second
	readyTimeout = 5 * time.Second
)

type runOpts struct {
	cfgFile  string
	cfgYAML  string
	cfgMap   map[string]any
	tbLogger bool
	lynxOpts []lynx.Option
}

// Option 配置 lynxtest.Run 的行为。
type Option func(*runOpts)

// WithConfigFile 以配置文件为基线加载（如生产 config.yaml），叠加顺序：
// 文件（低）→ WithConfigYAML → WithConfigMap（高）。复用生产配置、只覆盖
// 少数键时使用，避免整份复制 YAML 造成漂移。
func WithConfigFile(path string) Option {
	return func(o *runOpts) {
		o.cfgFile = path
	}
}

// WithConfigYAML 以 YAML 字符串注入配置（优先级高于 WithConfigFile、
// 低于 WithConfigMap），适合结构较深或需要注释的场景。
func WithConfigYAML(s string) Option {
	return func(o *runOpts) {
		o.cfgYAML = s
	}
}

// WithConfigMap 以键值对注入配置，优先级最高：测试里最常用的环境覆盖
// 入口，如 server.addr=":0"、registry.backend="memory"。
func WithConfigMap(m map[string]any) Option {
	return func(o *runOpts) {
		o.cfgMap = m
	}
}

// WithTBLogger 把应用日志接到 testing.TB 输出（-v 可见，随用例关联），
// 级别沿用配置的 logging.level（缺省 Info）。缺省不开：应用日志量通常
// 远大于框架事件日志，默认走 stderr 与生产一致。
func WithTBLogger() Option {
	return func(o *runOpts) {
		o.tbLogger = true
	}
}

// WithOptions 透传 lynx.Option，置于套件基线之后：可覆盖基线超时
// （如放慢 BusReadyTimeout/CleanupTimeout 适配慢后端与重清理）、设置
// 名称，或传入 lynx.WithIsolated() 以完全不触碰进程级全局（代价是依赖
// lynx.Get()/eventbus.Default() 的业务代码在测试里取不到本实例）。
func WithOptions(los ...lynx.Option) Option {
	return func(o *runOpts) {
		o.lynxOpts = append(o.lynxOpts, los...)
	}
}

// App 是 Run 返回的应用句柄：内嵌 lynx.App（全部既有方法可用），额外
// 提供测试感知的就绪等待——应用先于就绪退出时不再干等超时，立即以
// Run 的实际错误失败用例，避免 "not ready" 掩盖启动失败根因。
type App struct {
	lynx.App
	exited  chan struct{}
	exitErr error
}

// Exited 在 Run 返回（无论成败）后关闭，可多方安全等待。
func (a *App) Exited() <-chan struct{} { return a.exited }

// Err 返回 Run 的结果；仅当 Exited() 关闭后调用才有意义。
func (a *App) Err() error { return a.exitErr }

// WaitReady 等待所有 server 的 Ready channel 关闭（总预算为整个 timeout，
// 非每个 server 独立计时）；应用先退出则立即 Fatal 并附 Run 的实际错误。
// server 引用从 setup 闭包捕获。
func (a *App) WaitReady(t testing.TB, timeout time.Duration, servers ...lynx.Server) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, s := range servers {
		select {
		case <-s.Ready():
		case <-a.exited:
			t.Fatalf("lynxtest: app exited before server %q became ready: %v", s.Name(), a.Err())
		case <-deadline.C:
			t.Fatalf("lynxtest: server %q not ready within %s", s.Name(), timeout)
		}
	}
}

// Run 在测试进程内拉起完整 Lynx 应用：构造（含注入配置与快速超时基线）、
// 执行 setup（与生产 main 共用的组装函数）、后台 Run。
// t.Cleanup 里 Close 并等待 Run 返回——走与生产信号关停完全相同的序列
// （排水→OnPreStop→逆序 Stop→总线→OnPostStop）；Run 返回非 nil 错误
// 会以 t.Errorf 报告。进程级全局在清理时恢复为 Run 前的值。
//
// 已知取舍：基线 DrainTimeout=100ms 使测试态 HealthCheckers() 聚合恒含
// drainChecker（生产默认 DrainTimeout=0 时没有）；该窗口同时保证
// registry.Apply 一类 OnDrain 钩子有执行预算。
//
// 返回的 *App 可用于 Bus()/Config()/WaitReady 等断言；服务器地址经
// setup 捕获的 server 引用取（配合 HTTPClient/GRPCConn）。
func Run(t testing.TB, setup lynx.SetupFunc, opts ...Option) *App {
	t.Helper()
	if setup == nil {
		t.Fatal("lynxtest: setup must not be nil")
	}
	o := &runOpts{}
	for _, fn := range opts {
		fn(o)
	}

	base := []lynx.Option{
		lynx.WithStopTimeout(baseTimeout),
		lynx.WithShutdownTimeout(baseTimeout),
		lynx.WithBusReadyTimeout(baseTimeout),
		lynx.WithCleanupTimeout(baseTimeout),
		// 带 OnDrain 钩子的应用（如 registry.Apply）要求非零排水窗口，
		// 否则 Run 启动期直接失败；100ms 对无钩子应用也只是 100ms 关停等待。
		lynx.WithDrainTimeout(baseDrain),
	}

	// 配置叠加（低→高）：WithConfigFile → WithConfigYAML → WithConfigMap
	//（viper Set 优先级高于文件）。未提供任何源时注入空配置——裸 Run
	// 不读 os.Args、不搜工作目录。
	v := viper.New()
	if o.cfgFile != "" {
		v.SetConfigFile(o.cfgFile)
		if err := v.ReadInConfig(); err != nil {
			t.Fatalf("lynxtest: WithConfigFile read error: %v", err)
		}
	}
	if o.cfgYAML != "" {
		v.SetConfigType("yaml")
		if err := v.ReadConfig(strings.NewReader(o.cfgYAML)); err != nil {
			t.Fatalf("lynxtest: WithConfigYAML parse error: %v", err)
		}
	}
	if o.cfgMap != nil {
		for k, val := range o.cfgMap {
			v.Set(k, val)
		}
	}
	base = append(base, lynx.WithConfig(lynx.NewViperConfig(v)))
	base = append(base, o.lynxOpts...)

	// 基线不隔离全局：保存/恢复三个进程级全局，使依赖全局取值的业务代码
	// 在测试里与生产行为一致（见包注释的并行限制）。
	prevLynx := lynx.Get()
	prevBus := eventbus.Default()
	prevSlog := slog.Default()
	restore := func() {
		lynx.Set(prevLynx)
		eventbus.SetDefault(prevBus)
		slog.SetDefault(prevSlog)
	}

	app, err := lynx.NewApp(base...)
	if err != nil {
		restore()
		t.Fatalf("lynxtest: NewApp() error = %v", err)
	}

	if o.tbLogger {
		level := slog.LevelInfo
		if s := lynx.LogLevelFromConfig(app.Config()); s != "" {
			if lv, perr := lynx.ParseLogLevel(s); perr == nil {
				level = lv
			}
		}
		app.SetLogger(slog.New(slog.NewTextHandler(tbWriter{t: t}, &slog.HandlerOptions{Level: level})))
	}

	// cleanup 在 setup 之前注册：setup 失败或 panic（Goexit 也会执行已
	// 注册的 cleanup）时同样释放应用、恢复全局，不泄漏总线 goroutine。
	runStarted := false
	var closeOnce sync.Once
	handle := &App{App: app, exited: make(chan struct{})}
	t.Cleanup(func() {
		closeOnce.Do(func() {
			app.Close()
			if runStarted {
				select {
				case <-handle.exited:
					// ErrAppClosed 是合法交错：Close（本清理）先于后台 Run
					// goroutine 被调度时，Run 入口直接返回该哨兵错误。
					if err := handle.Err(); err != nil && !errors.Is(err, lynx.ErrAppClosed) {
						t.Errorf("lynxtest: app Run() = %v, want nil", err)
					}
				case <-time.After(runStopWait):
					t.Errorf("lynxtest: app Run() did not return within %s after Close", runStopWait)
				}
			}
		})
		restore()
	})

	if err := setup(app); err != nil {
		t.Fatalf("lynxtest: setup() error = %v", err)
	}
	runStarted = true
	go func() {
		err := app.Run()
		handle.exitErr = err
		close(handle.exited)
	}()
	return handle
}
