package lynxtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// tbWriter 把写入重定向到 testing.TB 的日志输出，使应用/服务日志直接
// 出现在测试输出里。注意：testing.TB 在用例结束后再写会 panic，与
// t.Log 的既有约束一致。
type tbWriter struct {
	t testing.TB
}

func (w tbWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// LogWriter 返回接到 testing.TB 的 io.Writer：应用侧 SetLogger 接 slog
// handler 时使用，例如
//
//	app.SetLogger(slog.New(slog.NewTextHandler(lynxtest.LogWriter(t), nil)))
func LogWriter(t testing.TB) io.Writer {
	return tbWriter{t: t}
}

const defaultBusReadyTimeout = 2 * time.Second

type contextConfig struct {
	cfg       lynx.Config
	cfgErr    error // Option 阶段的解析错误（如 YAML 非法），NewContext 统一 Fatal
	bus       eventbus.Bus
	logger    *slog.Logger
	readyWait time.Duration
	meta      lynx.Metadata
	checkers  []lynx.Checker
}

// ContextOption 配置 NewContext 的行为。
type ContextOption func(*contextConfig)

// ContextWithConfig 注入任意 lynx.Config 实现（含 koanf 等自定义后端的
// 包装）；与 ContextWithConfigMap/ContextWithConfigYAML 互斥，后设置者生效。
func ContextWithConfig(c lynx.Config) ContextOption {
	return func(o *contextConfig) {
		if c != nil {
			o.cfg = c
			o.cfgErr = nil
		}
	}
}

// ContextWithConfigMap 以键值对注入配置（语义同 Run 侧 WithConfigMap）。
func ContextWithConfigMap(m map[string]any) ContextOption {
	return func(o *contextConfig) {
		v := viper.New()
		for k, val := range m {
			v.Set(k, val)
		}
		o.cfg = lynx.NewViperConfig(v)
		o.cfgErr = nil
	}
}

// ContextWithConfigYAML 以 YAML 字符串注入配置；解析错误在 NewContext
// 里以测试失败报告，而不是 panic 击穿整个测试二进制。
func ContextWithConfigYAML(s string) ContextOption {
	return func(o *contextConfig) {
		v := viper.New()
		v.SetConfigType("yaml")
		if err := v.ReadConfig(strings.NewReader(s)); err != nil {
			o.cfgErr = fmt.Errorf("ContextWithConfigYAML parse error: %w", err)
			return
		}
		o.cfg = lynx.NewViperConfig(v)
		o.cfgErr = nil
	}
}

// ContextWithBus 注入自定义总线；缺省为内存总线（已 Init/Start，可立即
// Subscribe/Publish，用例结束后自动 Stop）。
func ContextWithBus(b eventbus.Bus) ContextOption {
	return func(o *contextConfig) {
		if b != nil {
			o.bus = b
		}
	}
}

// ContextWithLogger 设置日志实例；缺省为接到 testing.TB 的 Debug 级
// TextHandler。
func ContextWithLogger(l *slog.Logger) ContextOption {
	return func(o *contextConfig) {
		if l != nil {
			o.logger = l
		}
	}
}

// ContextWithMeta 注入应用元数据（lynx.Meta(ctx.Context()) 可见）；缺省
// {Name: "test-service", ID: "test-instance"}——与真实 App 一致，context
// 总是携带元数据（需要特定 service.name 的用例在此覆盖）。
func ContextWithMeta(meta lynx.Metadata) ContextOption {
	return func(o *contextConfig) {
		o.meta = meta
	}
}

// ContextWithCheckers 注入健康检查器快照（Service.Init 内
// ctx.HealthCheckers() 可见，用于驱动依赖健康聚合的代码路径）；缺省为空。
func ContextWithCheckers(cs ...lynx.Checker) ContextOption {
	return func(o *contextConfig) {
		o.checkers = cs
	}
}

// ContextWithBusReadyTimeout 设置总线就绪等待预算（缺省 2s）；注入慢
// 启动后端（Kafka 等）时放宽。
func ContextWithBusReadyTimeout(d time.Duration) ContextOption {
	return func(o *contextConfig) {
		if d > 0 {
			o.readyWait = d
		}
	}
}

// testContext 是 lynx.AppContext 的最小可用实现：真内存总线、注入配置、
// 接测试输出的日志、应用元数据与健康检查器快照（lynx.Meta 与
// HealthCheckers 语义与真实 App 对齐）。Close 取消 Context 并有界停止
// 总线（5s 上界，防注入的自定义总线 Stop 挂死拖住测试）。
type testContext struct {
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      lynx.Config
	bus      eventbus.Bus
	logger   *slog.Logger
	checkers []lynx.Checker
}

func (c *testContext) Context() context.Context          { return c.ctx }
func (c *testContext) Config() lynx.Config               { return c.cfg }
func (c *testContext) Logger(kwargs ...any) *slog.Logger { return c.logger.With(kwargs...) }
func (c *testContext) Bus() eventbus.Bus                 { return c.bus }
func (c *testContext) HealthCheckers() []lynx.Checker {
	return append([]lynx.Checker(nil), c.checkers...)
}
func (c *testContext) Close() {
	c.cancel()
	if c.bus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.bus.Stop(ctx)
}

var _ lynx.AppContext = (*testContext)(nil)

// NewContext 构造服务级单元测试用的 lynx.AppContext：内存总线走真实
// 生命周期（Init/Start，用例结束 Stop 后清理），配置、日志、应用元数据
// 与健康检查器快照可注入；缺省携带稳定 Meta。用于给单个 Service 的
// Init/Start/Stop 提供可用上下文，替代手写 fake（app 级注册协议与
// debug /loglevel 控制面例外，见 docs/design-testkit.md）。
func NewContext(t testing.TB, opts ...ContextOption) lynx.AppContext {
	t.Helper()
	c := &contextConfig{
		readyWait: defaultBusReadyTimeout,
		meta:      lynx.Metadata{Name: "test-service", ID: "test-instance"},
	}
	for _, fn := range opts {
		fn(c)
	}
	if c.cfgErr != nil {
		t.Fatalf("lynxtest: %v", c.cfgErr)
	}
	if c.cfg == nil {
		c.cfg = lynx.NewViperConfig(viper.New())
	}
	if c.bus == nil {
		c.bus = eventbus.NewMemoryBus(eventbus.Options{})
	}
	if c.logger == nil {
		c.logger = slog.New(slog.NewTextHandler(tbWriter{t: t}, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
	}

	ctx, cancel := context.WithCancel(context.Background())
	// 与真实 App 对齐：ctx 携带应用元数据，且内嵌总线——被测服务里
	// lynx.Meta 与 eventbus.BusFromContext 都拿到本上下文的值。
	ctx = lynx.ContextWithMeta(ctx, c.meta)
	ctx = eventbus.ContextWithBus(ctx, c.bus)
	tc := &testContext{
		ctx:      ctx,
		cancel:   cancel,
		cfg:      c.cfg,
		bus:      c.bus,
		logger:   c.logger,
		checkers: c.checkers,
	}
	// cleanup 前置：Init/就绪失败路径同样停总线、释放注入总线的资源。
	t.Cleanup(tc.Close)
	if err := c.bus.Init(tc); err != nil {
		cancel()
		t.Fatalf("lynxtest: bus Init() error = %v", err)
	}
	// 总线走真实生命周期（与 newLynx 一致：Start 阻塞至 ctx 取消，后台
	// 启动；有界等待就绪后返回，订阅与发布立即可用）。
	go func() { _ = c.bus.Start(ctx) }()
	deadline := time.Now().Add(c.readyWait)
	for time.Now().Before(deadline) {
		if c.bus.CheckHealth() == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.bus.CheckHealth(); err != nil {
		cancel()
		t.Fatalf("lynxtest: bus failed to become ready within %s: %v", c.readyWait, err)
	}
	return tc
}
