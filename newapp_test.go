package lynx

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// saveGlobals 记录三个进程级全局的当前值，返回恢复函数。
// 测试涉及全局注册路径时 defer 调用，避免污染同包其他用例。
func saveGlobals() func() {
	prevApp := Get()
	prevBus := eventbus.Default()
	prevSlog := slog.Default()
	return func() {
		Set(prevApp)
		eventbus.SetDefault(prevBus)
		slog.SetDefault(prevSlog)
	}
}

func TestNewApp_WithConfigInjects(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	v := viper.New()
	v.Set("service.name", "injected-app")
	v.Set("service.version", "9.9.9")
	cfg := NewViperConfig(v)

	app, err := NewApp(WithConfig(cfg), WithName("fallback"))
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()

	// 注入实例即权威配置：Config() 返回同一实例（证明走了注入路径，
	// flags/文件装配被跳过）。
	if app.Config() != Config(cfg) {
		t.Errorf("Config() did not return the injected Config instance")
	}
	if got := app.Config().GetString("service.name"); got != "injected-app" {
		t.Errorf("service.name = %q, want %q", got, "injected-app")
	}
	// 元数据优先取配置值，与默认路径语义一致。
	if m := Meta(app.Context()); m.Name != "injected-app" || m.Version != "9.9.9" {
		t.Errorf("Meta() = %+v, want name=%q version=%q", m, "injected-app", "9.9.9")
	}
}

func TestNewApp_WithConfigNilIsNoop(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	app, err := NewApp(WithConfig(nil), WithName("nil-config"))
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()

	// nil 注入无效：走默认装配路径，Config() 不是 nil 即可。
	if app.Config() == nil {
		t.Errorf("Config() = nil, want default viper-backed config")
	}
}

func TestNewApp_WithIsolatedSkipsGlobals(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	app, err := NewApp(WithIsolated(), WithName("isolated"))
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()

	if Get() != nil && Get() == App(app) {
		t.Errorf("isolated app registered itself into lynx.Get()")
	}
	if eventbus.Default() == app.Bus() {
		t.Errorf("isolated app registered its bus into eventbus.Default()")
	}

	// logger 定制路径的 slog.SetDefault 副作用同样被隔离。
	prevSlog := slog.Default()
	app.(*lynx).setLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if slog.Default() != prevSlog {
		t.Errorf("isolated app setLogger changed slog default")
	}
}

func TestNewApp_DefaultRegistersGlobals(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	app, err := NewApp(WithName("global"))
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()

	if Get() != App(app) {
		t.Errorf("lynx.Get() did not return the app (default path should register)")
	}
	if eventbus.Default() != app.Bus() {
		t.Errorf("eventbus.Default() did not return the app bus (default path should register)")
	}
}

// TestNewApp_MatchesRunnerOptionSemantics 回归：NewApp 与 NewRunner 的
// opts 应用顺序必须一致（空 Options → 应用 opts → newLynx 内 EnsureDefaults）。
// 顺序敏感的 WithBusOptions 依赖 o.Bus 尚未被默认值填充，若 NewApp 走
// NewOptions（defaults-first）会把该 Option 静默丢弃。
// 哨兵：经 WithBusOptions 注入自定义 Marshaler，经导出的 MarshalerFor 观察。
type sentinelMarshaler struct {
	eventbus.JSONMarshaler
}

func TestNewApp_MatchesRunnerOptionSemantics(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	want := sentinelMarshaler{}
	app, err := NewApp(WithBusOptions(eventbus.WithMarshaler(want)))
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	defer app.Close()

	if got := app.Bus().MarshalerFor("any.topic"); got != eventbus.Marshaler(want) {
		t.Errorf("MarshalerFor() = %T, want sentinelMarshaler (WithBusOptions was silently dropped)", got)
	}
}

func TestNewApp_RunAndCloseLifecycle(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	v := viper.New()
	v.Set("service.name", "run-cycle")
	app, err := NewApp(
		WithConfig(NewViperConfig(v)),
		WithStopTimeout(time.Second),
		WithShutdownTimeout(time.Second),
		WithBusReadyTimeout(time.Second),
		WithDrainTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}

	svc := &blockingService{name: "m"}
	app.Register(svc)

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()

	// 等待服务真正进入运行态，保证 Close 走"Run 已启动"路径。
	deadline := time.Now().Add(3 * time.Second)
	for !svc.started.Load() {
		if time.Now().After(deadline) {
			t.Fatal("service did not start within 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s after Close()")
	}
}
