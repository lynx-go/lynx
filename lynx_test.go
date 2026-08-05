package lynx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingComponent blocks in Start until its context is cancelled.
type blockingComponent struct {
	name    string
	record  func(string)
	started atomic.Bool
}

func (c *blockingComponent) Name() string { return c.name }

func (c *blockingComponent) Init(app App) error { return nil }

func (c *blockingComponent) Start(ctx context.Context) error {
	c.started.Store(true)
	if c.record != nil {
		c.record("start:" + c.name)
	}
	<-ctx.Done()
	return nil
}

func (c *blockingComponent) Stop(ctx context.Context) {
	if c.record != nil {
		c.record("stop:" + c.name)
	}
}

// failInitComponent fails in Init.
type failInitComponent struct {
	name string
	err  error
}

func (c *failInitComponent) Name() string                    { return c.name }
func (c *failInitComponent) Init(app App) error              { return c.err }
func (c *failInitComponent) Start(ctx context.Context) error { return nil }
func (c *failInitComponent) Stop(ctx context.Context)        {}

// failStartComponent fails in Start.
type failStartComponent struct {
	name string
	err  error
}

func (c *failStartComponent) Name() string                    { return c.name }
func (c *failStartComponent) Init(app App) error              { return nil }
func (c *failStartComponent) Start(ctx context.Context) error { return c.err }
func (c *failStartComponent) Stop(ctx context.Context)        {}

// checkerComponent implements both Component and health.Checker.
type checkerComponent struct {
	HealthChecker
	name string
}

func (c *checkerComponent) Name() string       { return c.name }
func (c *checkerComponent) Init(app App) error { return nil }
func (c *checkerComponent) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (c *checkerComponent) Stop(ctx context.Context) {}

// initRecorder records whether Init was called.
type initRecorder struct {
	name        string
	initialized atomic.Bool
}

func (c *initRecorder) Name() string { return c.name }
func (c *initRecorder) Init(app App) error {
	c.initialized.Store(true)
	return nil
}
func (c *initRecorder) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (c *initRecorder) Stop(ctx context.Context) {}

// recordingBuilder builds blockingComponents and counts Build calls.
type recordingBuilder struct {
	instances int
	builds    atomic.Int32
}

func (b *recordingBuilder) Build() Component {
	b.builds.Add(1)
	return &blockingComponent{name: "built"}
}

func (b *recordingBuilder) Options() BuildOptions {
	return BuildOptions{Instances: b.instances}
}

// eventRecorder records events in a concurrency-safe way.
type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *eventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

// initAppAccessorComponent 在 Init 中调用需要 app.mu 的 App 方法：
// Init 若在持锁时执行（旧的 addComponents 路径）会死锁。
type initAppAccessorComponent struct{}

func (c *initAppAccessorComponent) Name() string { return "accessor" }
func (c *initAppAccessorComponent) Init(app App) error {
	app.HealthCheckFunc()
	app.OnStart(func(ctx context.Context) error { return nil })
	app.OnStop(func(ctx context.Context) error { return nil })
	app.Config()
	app.Context()
	return nil
}
func (c *initAppAccessorComponent) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *initAppAccessorComponent) Stop(ctx context.Context)        {}

// stopRecorder 在 Stop 时向缓冲 chan 发送组件名，用于失败清理断言。
type stopRecorder struct {
	name    string
	stopped chan string
}

func (c *stopRecorder) Name() string                    { return c.name }
func (c *stopRecorder) Init(app App) error              { return nil }
func (c *stopRecorder) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *stopRecorder) Stop(ctx context.Context)        { c.stopped <- c.name }

// hangStopComponent 的 Stop 永不返回，用于验证 StopTimeout 有界兜底。
type hangStopComponent struct{ name string }

func (c *hangStopComponent) Name() string                    { return c.name }
func (c *hangStopComponent) Init(app App) error              { return nil }
func (c *hangStopComponent) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *hangStopComponent) Stop(ctx context.Context)        { select {} }

func TestInitCanCallAppMethods(t *testing.T) {
	cli := NewBuilder(func(ctx context.Context, app App) error {
		app.Register(&initAppAccessorComponent{})
		return nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		cli.Build()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Init deadlocked while calling App methods (Init must run outside app.mu)")
	}
}

func TestInitFailureStopsPreviouslyInitializedComponents(t *testing.T) {
	stopped := make(chan string, 10)
	good := &stopRecorder{name: "good", stopped: stopped}
	bad := &failInitComponent{name: "bad", err: errors.New("init boom")}
	cli := NewBuilder(func(ctx context.Context, app App) error {
		app.Register(good, bad)
		return nil
	})
	if err := cli.RunE(); err == nil {
		t.Fatal("expected init error")
	}
	select {
	case name := <-stopped:
		if name != "good" {
			t.Fatalf("stopped %q, want good", name)
		}
	case <-time.After(time.Second):
		t.Fatal("previously initialized component was not stopped after init failure")
	}
}

func TestOnStartHookErrorStopsInitializedComponents(t *testing.T) {
	stopped := make(chan string, 10)
	comp := &stopRecorder{name: "comp", stopped: stopped}
	cli := NewBuilder(func(ctx context.Context, app App) error {
		app.Register(comp)
		app.OnStart(func(ctx context.Context) error { return errors.New("hook boom") })
		return nil
	})
	if err := cli.RunE(); err == nil {
		t.Fatal("expected on-start hook error")
	}
	select {
	case name := <-stopped:
		if name != "comp" {
			t.Fatalf("stopped %q, want comp", name)
		}
	case <-time.After(time.Second):
		t.Fatal("component was not stopped after on-start hook failure")
	}
}

func TestRunReturnsOnStopHookErrors(t *testing.T) {
	cli := NewBuilder(func(ctx context.Context, app App) error {
		app.Register(&blockingComponent{name: "c"})
		app.OnStop(func(ctx context.Context) error { return errors.New("drain failed") })
		return nil
	})
	app := cli.Build()
	go func() {
		time.Sleep(50 * time.Millisecond)
		app.Close()
	}()
	err := cli.RunE()
	if err == nil || !strings.Contains(err.Error(), "drain failed") {
		t.Fatalf("Run() error = %v, want drain failed", err)
	}
}

func TestComponentStopBoundedByTimeout(t *testing.T) {
	cli := NewBuilder(func(ctx context.Context, app App) error {
		app.Register(&hangStopComponent{name: "hang"})
		return nil
	}, WithStopTimeout(time.Second))
	app := cli.Build()
	go func() {
		time.Sleep(50 * time.Millisecond)
		app.Close()
	}()
	start := time.Now()
	if err := cli.RunE(); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown hung: elapsed %v, want bounded by StopTimeout", elapsed)
	}
}

func TestBuilderNilBuildFunc(t *testing.T) {
	b := NewBuilder(nil)
	if app := b.Build(); app != nil {
		t.Fatalf("Build() = %v, want nil", app)
	}
	if err := b.RunE(); !errors.Is(err, ErrBuildFuncNil) {
		t.Fatalf("RunE() error = %v, want ErrBuildFuncNil", err)
	}
}

func TestBuilderBuildReturnsNilAfterCallbackFailure(t *testing.T) {
	calls := 0
	b := NewBuilder(func(ctx context.Context, app App) error {
		calls++
		return errors.New("build boom")
	})
	if app := b.Build(); app != nil {
		t.Fatalf("first Build() = %v, want nil", app)
	}
	if app := b.Build(); app != nil {
		t.Fatalf("second Build() = %v, want nil (consistent contract after failure)", app)
	}
	if calls != 1 {
		t.Fatalf("build called %d times, want 1", calls)
	}
}

func TestNewLynx(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	if app.Config() == nil {
		t.Error("Config() should not be nil")
	}
	if app.Context() == nil {
		t.Error("Context() should not be nil")
	}
	if app.Logger() == nil {
		t.Error("Logger() should not be nil")
	}
}

func TestRegisterInitErrorSurfacesAtRun(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	wantErr := errors.New("init failed")
	app.Register(&failInitComponent{name: "bad", err: wantErr})

	if err := app.Run(); !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

// TestRegisterSkippedAfterInitError verifies the poison-pill semantics:
// once a registration fails, later registrations are skipped so that
// dependent components are not initialized against a broken state.
func TestRegisterSkippedAfterInitError(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	app.Register(&failInitComponent{name: "bad", err: errors.New("init failed")})

	second := &initRecorder{name: "second"}
	app.Register(second)
	if second.initialized.Load() {
		t.Error("second component should not be initialized after a failed registration")
	}

	builder := &recordingBuilder{instances: 1}
	app.RegisterBuilders(builder)
	if got := builder.builds.Load(); got != 0 {
		t.Errorf("Build() called %d times after a failed registration, want 0", got)
	}
}

func TestComponentBuildersInstances(t *testing.T) {
	tests := []struct {
		name       string
		instances  int
		wantBuilds int32
	}{
		{name: "zero instances defaults to one", instances: 0, wantBuilds: 1},
		{name: "negative instances defaults to one", instances: -3, wantBuilds: 1},
		{name: "one instance", instances: 1, wantBuilds: 1},
		{name: "three instances", instances: 3, wantBuilds: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, err := newLynx(NewOptions())
			if err != nil {
				t.Fatalf("newLynx() error = %v", err)
			}
			builder := &recordingBuilder{instances: tt.instances}
			app.RegisterBuilders(builder)
			if got := builder.builds.Load(); got != tt.wantBuilds {
				t.Errorf("Build() called %d times, want %d", got, tt.wantBuilds)
			}
		})
	}
}

func TestHealthCheckersRegistered(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	if got := len(app.HealthCheckFunc()()); got != 0 {
		t.Fatalf("health checkers = %d, want 0 before registering components", got)
	}

	comp := &checkerComponent{name: "checker"}
	app.Register(comp)

	checkers := app.HealthCheckFunc()()
	if len(checkers) != 1 {
		t.Fatalf("health checkers = %d, want 1", len(checkers))
	}
	if checkers[0] != comp {
		t.Error("registered health checker is not the component")
	}

	// A component that is not a health.Checker must not be registered.
	app.Register(&blockingComponent{name: "plain"})
	if got := len(app.HealthCheckFunc()()); got != 1 {
		t.Errorf("health checkers = %d, want 1 after adding non-checker component", got)
	}
}

func TestRunLifecycleStartStopOrdering(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	c1 := &blockingComponent{name: "c1", record: rec.record}
	c2 := &blockingComponent{name: "c2", record: rec.record}
	app.Register(c1, c2)
	app.OnStop(func(ctx context.Context) error {
		rec.record("onstop")
		return nil
	})

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	waitFor(t, 2*time.Second, func() bool {
		return c1.started.Load() && c2.started.Load()
	}, "components to start")

	app.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}

	events := rec.snapshot()

	// Both components must have started (start order is concurrent, so check membership).
	started := map[string]bool{}
	for _, e := range events {
		if e == "start:c1" || e == "start:c2" {
			started[e] = true
		}
	}
	if !started["start:c1"] || !started["start:c2"] {
		t.Errorf("events = %v, want both components started", events)
	}

	// Shutdown ordering is deterministic: OnStop hooks run before components
	// stop, and components stop in registration order.
	var stops []string
	for _, e := range events {
		if e == "stop:c1" || e == "stop:c2" || e == "onstop" {
			stops = append(stops, e)
		}
	}
	want := []string{"onstop", "stop:c1", "stop:c2"}
	if len(stops) != len(want) {
		t.Fatalf("stop events = %v, want %v", stops, want)
	}
	for i := range want {
		if stops[i] != want[i] {
			t.Fatalf("stop events = %v, want %v", stops, want)
		}
	}
}

func TestRunOnStartHookError(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	wantErr := errors.New("on-start failed")
	app.OnStart(func(ctx context.Context) error {
		return wantErr
	})

	if err := app.Run(); !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

func TestRunComponentStartError(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	wantErr := errors.New("start failed")
	app.Register(&failStartComponent{name: "bad", err: wantErr})

	if err := app.Run(); !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

func TestRunOnStopHooksAllExecutedDespiteErrors(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	var mu sync.Mutex
	var executed []string
	record := func(name string) HookFunc {
		return func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			executed = append(executed, name)
			if name != "hook3" {
				return errors.New(name + " failed")
			}
			return nil
		}
	}
	app.OnStop(record("hook1"), record("hook2"), record("hook3"))

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	// Give Run a moment to register its actors, then shut down.
	time.Sleep(50 * time.Millisecond)
	app.Close()

	select {
	case err := <-runErr:
		// A4: OnStop 错误随 Run() 上抛，让调用方（如 K8s）感知关停失败。
		if err == nil {
			t.Fatalf("Run() error = nil, want shutdown errors to surface")
		}
		if !strings.Contains(err.Error(), "hook1 failed") ||
			!strings.Contains(err.Error(), "hook2 failed") {
			t.Fatalf("Run() error = %v, want hook1/hook2 failures", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"hook1", "hook2", "hook3"}
	if len(executed) != len(want) {
		t.Fatalf("executed = %v, want %v", executed, want)
	}
	for i := range want {
		if executed[i] != want[i] {
			t.Fatalf("executed = %v, want %v", executed, want)
		}
	}
}

func TestCLICommandRunsAndClosesApp(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	var ran atomic.Int32
	if err := app.CLI(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("CLI() error = %v", err)
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after command finished")
	}

	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}

	// The command's Stop closes the app.
	select {
	case <-app.Context().Done():
	default:
		t.Error("app context should be cancelled after command completes")
	}
}

func TestCloseCancelsContext(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	select {
	case <-app.Context().Done():
		t.Fatal("context should not be cancelled before Close()")
	default:
	}
	app.Close()
	select {
	case <-app.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("context should be cancelled after Close()")
	}
}

func TestSetLogger(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app.SetLogger(logger)
	if app.Logger() != logger {
		t.Error("Logger() should return the logger set via SetLogger")
	}
}

// TestConfigFileNotFoundIsNotFatal 验证未显式指定配置文件且不存在配置文件时
// 应用仍能启动（配置是可选的），即修复 C1。
func TestConfigFileNotFoundIsNotFatal(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx"}

	if _, err := newLynx(NewOptions(WithUseDefaultConfigFlagsFunc())); err != nil {
		t.Fatalf("newLynx() error = %v, want nil when no config file is present", err)
	}
}

// TestExplicitConfigFileMissingIsFatal 验证显式指定的配置文件缺失时仍为硬错误。
func TestExplicitConfigFileMissingIsFatal(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-c", "does-not-exist.yaml"}

	if _, err := newLynx(NewOptions(WithUseDefaultConfigFlagsFunc())); err == nil {
		t.Fatal("newLynx() error = nil, want error when explicit config file is missing")
	}
}

// TestOnStartRunsBeforeComponentsStart 验证 OnStart hooks 在组件启动前顺序执行。
func TestOnStartRunsBeforeComponentsStart(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	c := &blockingComponent{name: "c1", record: rec.record}
	app.Register(c)
	app.OnStart(func(ctx context.Context) error {
		if c.started.Load() {
			t.Error("OnStart hook ran after component had already started")
		}
		rec.record("onstart")
		return nil
	})

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	waitFor(t, 2*time.Second, func() bool { return c.started.Load() }, "component to start")
	app.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}

	events := rec.snapshot()
	if len(events) == 0 || events[0] != "onstart" {
		t.Fatalf("events = %v, want onstart recorded before component start", events)
	}
}

// TestOnStopHookBlockingBoundedByTimeout 验证忽略 ctx 的阻塞 OnStop hook
// 不会挂起关闭流程，总时长受 ShutdownTimeout 约束（M3）。
func TestOnStopHookBlockingBoundedByTimeout(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	// 白盒收紧超时，避免 Options.Validate 的 MinShutdownTimeout 限制。
	app.(*lynx).o.ShutdownTimeout = 100 * time.Millisecond

	app.OnStop(func(ctx context.Context) error {
		time.Sleep(5 * time.Second) // 故意忽略 ctx
		return nil
	})

	start := time.Now()
	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()
	time.Sleep(50 * time.Millisecond)
	app.Close()

	select {
	case err := <-runErr:
		// A4: 超时错误随 Run() 上抛，调用方可感知关停失败。
		if err == nil || !strings.Contains(err.Error(), "on-stop hook timed out") {
			t.Fatalf("Run() error = %v, want on-stop hook timed out", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return; blocking OnStop hook was not bounded")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdown took %v, want bounded by shutdown timeout", elapsed)
	}
}

// TestBuilderBuildIsIdempotent 验证 Build() 只运行一次构建回调（M6）。
func TestBuilderBuildIsIdempotent(t *testing.T) {
	var calls atomic.Int32
	builder := NewBuilder(func(ctx context.Context, app App) error {
		calls.Add(1)
		return nil
	})
	b := builder.Build()
	if b == nil {
		t.Fatal("Build() returned nil")
	}
	b2 := builder.Build()
	if b2 != b {
		t.Error("Build() should return the same app on subsequent calls")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("build callback called %d times, want 1", got)
	}
}

// TestBuilderRunEReturnsInitError 验证 newLynx 初始化失败（如 Options 校验）
// 通过 RunE 返回而非直接退出进程（L7）。
func TestBuilderRunEReturnsInitError(t *testing.T) {
	builder := NewBuilder(func(ctx context.Context, app App) error { return nil },
		WithName(strings.Repeat("a", 64))) // 触发 ErrNameTooLong
	if err := builder.RunE(); !errors.Is(err, ErrNameTooLong) {
		t.Fatalf("RunE() error = %v, want ErrNameTooLong", err)
	}
}

// TestCommandStartBeforeInit 验证未注册的 command 直接 Start 返回 ErrNotInitialized（L3）。
func TestCommandStartBeforeInit(t *testing.T) {
	cmd := NewCommand(func(ctx context.Context) error { return nil })
	if err := cmd.Start(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Start() error = %v, want ErrNotInitialized", err)
	}
}

// TestParseLogLevel 验证日志级别解析（M12）。
func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for _, tt := range tests {
		lvl, err := parseLogLevel(tt.in)
		if err != nil || lvl != tt.want {
			t.Errorf("parseLogLevel(%q) = %v, %v; want %v", tt.in, lvl, err, tt.want)
		}
	}
	if _, err := parseLogLevel("bogus"); err == nil {
		t.Error("parseLogLevel(\"bogus\") should return an error")
	}
}
