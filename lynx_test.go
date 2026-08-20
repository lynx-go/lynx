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

	"github.com/oklog/run"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// blockingService blocks in Start until its context is cancelled.
type blockingService struct {
	name    string
	record  func(string)
	started atomic.Bool
}

func (c *blockingService) Name() string { return c.name }

func (c *blockingService) Init(ctx AppContext) error { return nil }

func (c *blockingService) Start(ctx context.Context) error {
	c.started.Store(true)
	if c.record != nil {
		c.record("start:" + c.name)
	}
	<-ctx.Done()
	return nil
}

func (c *blockingService) Stop(ctx context.Context) error {
	if c.record != nil {
		c.record("stop:" + c.name)
	}
	return nil
}

// failInitService fails in Init.
type failInitService struct {
	name string
	err  error
}

func (c *failInitService) Name() string                    { return c.name }
func (c *failInitService) Init(ctx AppContext) error       { return c.err }
func (c *failInitService) Start(ctx context.Context) error { return nil }
func (c *failInitService) Stop(ctx context.Context) error  { return nil }

// failStartService fails in Start.
type failStartService struct {
	name string
	err  error
}

func (c *failStartService) Name() string                    { return c.name }
func (c *failStartService) Init(ctx AppContext) error       { return nil }
func (c *failStartService) Start(ctx context.Context) error { return c.err }
func (c *failStartService) Stop(ctx context.Context) error  { return nil }

// checkerService implements both Service and Checker.
type checkerService struct {
	HealthChecker
	name string
}

func (c *checkerService) Name() string              { return c.name }
func (c *checkerService) Init(ctx AppContext) error { return nil }
func (c *checkerService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (c *checkerService) Stop(ctx context.Context) error { return nil }

// initRecorder records whether Init was called.
type initRecorder struct {
	name        string
	initialized atomic.Bool
}

func (c *initRecorder) Name() string { return c.name }
func (c *initRecorder) Init(ctx AppContext) error {
	c.initialized.Store(true)
	return nil
}
func (c *initRecorder) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (c *initRecorder) Stop(ctx context.Context) error { return nil }

// recordingFactory builds blockingServices and counts New calls.
type recordingFactory struct {
	instances int
	builds    atomic.Int32
}

func (b *recordingFactory) New() Service {
	b.builds.Add(1)
	return &blockingService{name: "built"}
}

func (b *recordingFactory) Options() FactoryOptions {
	return FactoryOptions{Instances: b.instances}
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

// initAppAccessorService 在 Init 中调用需要 app.mu 的 App 方法：
// Init 若在持锁时执行（旧的 addServices 路径）会死锁。
type initAppAccessorService struct{}

func (c *initAppAccessorService) Name() string { return "accessor" }
func (c *initAppAccessorService) Init(ctx AppContext) error {
	ctx.HealthCheckers()
	app := ctx.(App)
	app.OnStart(func(ctx context.Context) error { return nil })
	app.OnStop(func(ctx context.Context) error { return nil })
	ctx.Config()
	ctx.Context()
	return nil
}
func (c *initAppAccessorService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *initAppAccessorService) Stop(ctx context.Context) error  { return nil }

// stopRecorder 在 Stop 时向缓冲 chan 发送服务名，用于失败清理断言。
type stopRecorder struct {
	name    string
	stopped chan string
}

func (c *stopRecorder) Name() string                    { return c.name }
func (c *stopRecorder) Init(ctx AppContext) error       { return nil }
func (c *stopRecorder) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *stopRecorder) Stop(ctx context.Context) error {
	c.stopped <- c.name
	return nil
}

// hangStopService 的 Stop 永不返回，用于验证 StopTimeout 有界兜底。
type hangStopService struct{ name string }

func (c *hangStopService) Name() string                    { return c.name }
func (c *hangStopService) Init(ctx AppContext) error       { return nil }
func (c *hangStopService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *hangStopService) Stop(ctx context.Context) error  { select {} }

// failStopService 的 Stop 返回错误，用于验证关停错误随 Run() 上抛。
type failStopService struct{ name string }

func (c *failStopService) Name() string                    { return c.name }
func (c *failStopService) Init(ctx AppContext) error       { return nil }
func (c *failStopService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *failStopService) Stop(ctx context.Context) error  { return errors.New("stop boom") }

// slowInitService 的 Init 阻塞直到 release 放行，用于在 Register 与 Run
// 之间制造确定性交错（Register/Run 并发裁决的回归测试）。
type slowInitService struct {
	name        string
	release     chan struct{}
	enteredInit chan struct{}
	started     atomic.Bool
	stopped     atomic.Bool
}

func (c *slowInitService) Name() string { return c.name }

func (c *slowInitService) Init(ctx AppContext) error {
	close(c.enteredInit)
	<-c.release
	return nil
}

func (c *slowInitService) Start(ctx context.Context) error {
	c.started.Store(true)
	<-ctx.Done()
	return nil
}

func (c *slowInitService) Stop(ctx context.Context) error { c.stopped.Store(true); return nil }

// drainProbe 记录 Start 进入与返回的时刻，用于断言服务在排水窗口内保持运行。
type drainProbe struct {
	name        string
	started     atomic.Bool
	startReturn atomic.Int64 // unix nano
	stopped     atomic.Bool
}

func (c *drainProbe) Name() string { return c.name }
func (c *drainProbe) Init(ctx AppContext) error {
	return nil
}
func (c *drainProbe) Start(ctx context.Context) error {
	c.started.Store(true)
	<-ctx.Done()
	c.startReturn.Store(time.Now().UnixNano())
	return nil
}
func (c *drainProbe) Stop(ctx context.Context) error {
	c.stopped.Store(true)
	return nil
}

func TestInitCanCallAppMethods(t *testing.T) {
	runner := NewRunner(func(app App) error {
		app.Register(&initAppAccessorService{})
		return nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runner.setupApp()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Init deadlocked while calling App methods (Init must run outside app.mu)")
	}
}

func TestInitFailureStopsPreviouslyInitializedServices(t *testing.T) {
	stopped := make(chan string, 10)
	good := &stopRecorder{name: "good", stopped: stopped}
	bad := &failInitService{name: "bad", err: errors.New("init boom")}
	runner := NewRunner(func(app App) error {
		app.Register(good, bad)
		return nil
	})
	if err := runner.RunE(); err == nil {
		t.Fatal("expected init error")
	}
	select {
	case name := <-stopped:
		if name != "good" {
			t.Fatalf("stopped %q, want good", name)
		}
	case <-time.After(time.Second):
		t.Fatal("previously initialized Service was not stopped after init failure")
	}
}

func TestOnStartHookErrorStopsInitializedServices(t *testing.T) {
	stopped := make(chan string, 10)
	comp := &stopRecorder{name: "comp", stopped: stopped}
	runner := NewRunner(func(app App) error {
		app.Register(comp)
		app.OnStart(func(ctx context.Context) error { return errors.New("hook boom") })
		return nil
	})
	if err := runner.RunE(); err == nil {
		t.Fatal("expected on-start hook error")
	}
	select {
	case name := <-stopped:
		if name != "comp" {
			t.Fatalf("stopped %q, want comp", name)
		}
	case <-time.After(time.Second):
		t.Fatal("Service was not stopped after on-start hook failure")
	}
}

func TestRunReturnsOnStopHookErrors(t *testing.T) {
	runner := NewRunner(func(app App) error {
		app.Register(&blockingService{name: "c"})
		app.OnStop(func(ctx context.Context) error { return errors.New("drain failed") })
		return nil
	})
	app, err := runner.setupApp()
	if err != nil {
		t.Fatalf("setupApp() error = %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		app.Close()
	}()
	err = runner.RunE()
	if err == nil || !strings.Contains(err.Error(), "drain failed") {
		t.Fatalf("Run() error = %v, want drain failed", err)
	}
}

func TestServiceStopBoundedByTimeout(t *testing.T) {
	runner := NewRunner(func(app App) error {
		app.Register(&hangStopService{name: "hang"})
		return nil
	}, WithStopTimeout(time.Second))
	app, err := runner.setupApp()
	if err != nil {
		t.Fatalf("setupApp() error = %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		app.Close()
	}()
	start := time.Now()
	err = runner.RunE()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown hung: elapsed %v, want bounded by StopTimeout", elapsed)
	}
	// 挂死服务的超时错误随 Run() 上抛（关停错误对称上抛）。
	if err == nil || !strings.Contains(err.Error(), "stop timed out") {
		t.Fatalf("Run() error = %v, want stop timed out error", err)
	}
}

// TestServiceStopErrorSurfacesAtRun 验证服务 Stop 返回的错误随 Run()
// 统一上抛（关停错误对称上抛）。
func TestServiceStopErrorSurfacesAtRun(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.Register(&failStopService{name: "bad-stop"})

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()
	time.Sleep(50 * time.Millisecond)
	app.Close()

	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), "stop boom") {
			t.Fatalf("Run() error = %v, want Service stop error to surface", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
}

func TestRunnerNilSetupFunc(t *testing.T) {
	b := NewRunner(nil)
	if app, err := b.setupApp(); app != nil || !errors.Is(err, ErrSetupFuncNil) {
		t.Fatalf("setupApp() = %v, %v; want nil, ErrSetupFuncNil", app, err)
	}
	if err := b.RunE(); !errors.Is(err, ErrSetupFuncNil) {
		t.Fatalf("RunE() error = %v, want ErrSetupFuncNil", err)
	}
}

func TestRunnerSetupReturnsNilAfterCallbackFailure(t *testing.T) {
	calls := 0
	b := NewRunner(func(app App) error {
		calls++
		return errors.New("setup boom")
	})
	if app, err := b.setupApp(); app != nil || err == nil {
		t.Fatalf("first setupApp() = %v, %v; want nil, error", app, err)
	}
	if app, err := b.setupApp(); app != nil || err == nil {
		t.Fatalf("second setupApp() = %v, %v; want nil, error (consistent contract after failure)", app, err)
	}
	if calls != 1 {
		t.Fatalf("setup called %d times, want 1", calls)
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
	app.Register(&failInitService{name: "bad", err: wantErr})

	if err := app.Run(); !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

// TestRegisterSkippedAfterInitError verifies the poison-pill semantics:
// once a registration fails, later registrations are skipped so that
// dependent Services are not initialized against a broken state.
func TestRegisterSkippedAfterInitError(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	app.Register(&failInitService{name: "bad", err: errors.New("init failed")})

	second := &initRecorder{name: "second"}
	app.Register(second)
	if second.initialized.Load() {
		t.Error("second Service should not be initialized after a failed registration")
	}

	factory := &recordingFactory{instances: 1}
	app.RegisterFactories(factory)
	if got := factory.builds.Load(); got != 0 {
		t.Errorf("New() called %d times after a failed registration, want 0", got)
	}
}

func TestServiceFactoriesInstances(t *testing.T) {
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
			factory := &recordingFactory{instances: tt.instances}
			app.RegisterFactories(factory)
			if got := factory.builds.Load(); got != tt.wantBuilds {
				t.Errorf("New() called %d times, want %d", got, tt.wantBuilds)
			}
		})
	}
}

func TestHealthCheckersRegistered(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	if got := len(app.HealthCheckers()); got != 0 {
		t.Fatalf("health checkers = %d, want 0 before registering Services", got)
	}

	comp := &checkerService{name: "checker"}
	app.Register(comp)

	checkers := app.HealthCheckers()
	if len(checkers) != 1 {
		t.Fatalf("health checkers = %d, want 1", len(checkers))
	}
	if checkers[0] != comp {
		t.Error("registered health checker is not the Service")
	}

	// A Service that is not a Checker must not be registered.
	app.Register(&blockingService{name: "plain"})
	if got := len(app.HealthCheckers()); got != 1 {
		t.Errorf("health checkers = %d, want 1 after adding non-checker Service", got)
	}
}

// TestRegisterNilService 验证 plain nil 服务注册返回明确错误而非运行时 panic。
func TestRegisterNilService(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.Register(nil)
	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "cannot register nil service") {
		t.Fatalf("Run() error = %v, want cannot-register-nil error", err)
	}
}

func TestRunLifecycleStartStopOrdering(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	c1 := &blockingService{name: "c1", record: rec.record}
	c2 := &blockingService{name: "c2", record: rec.record}
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
	}, "Services to start")

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

	// Both Services must have started (start order is concurrent, so check membership).
	started := map[string]bool{}
	for _, e := range events {
		if e == "start:c1" || e == "start:c2" {
			started[e] = true
		}
	}
	if !started["start:c1"] || !started["start:c2"] {
		t.Errorf("events = %v, want both Services started", events)
	}

	// Shutdown ordering is deterministic: OnStop hooks run before Services
	// stop, and Services stop in registration order.
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

func TestRunServiceStartError(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	wantErr := errors.New("start failed")
	app.Register(&failStartService{name: "bad", err: wantErr})

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
	if err := app.Command(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Command() error = %v", err)
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

// TestRegisterAfterRunRejected 回归：Run 开始后注册为禁止操作——
// Register/RegisterFactories panic 报明确错误，Command 返回错误；
// 晚到的注册不得触碰 run.Group 的 actors（此前为 data race 且服务
// 永不 Start 却被 Stop）。
func TestRegisterAfterRunRejected(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	c := &blockingService{name: "c"}
	app.Register(c)

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return c.started.Load() }, "Service to start")

	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("%s did not panic after Run() started", name)
			}
		}()
		fn()
	}
	assertPanics("Register", func() { app.Register(&blockingService{name: "late"}) })
	assertPanics("RegisterFactories", func() { app.RegisterFactories(&recordingFactory{instances: 1}) })
	if err := app.Command(func(ctx context.Context) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "must not be called after Run") {
		t.Fatalf("Command() error = %v, want explicit after-Run error", err)
	}

	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
}

// TestRunJoinsOnStopErrorsWithStartFailure 回归：服务 Start 先失败时，
// oklog/run 只返回首个 actor 错误，OnStop 钩子错误必须与之一并上抛，
// 不得只落日志。
func TestRunJoinsOnStopErrorsWithStartFailure(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	app.Register(&failStartService{name: "bad", err: errors.New("start boom")})
	app.OnStop(func(ctx context.Context) error { return errors.New("onstop boom") })

	err = app.Run()
	if err == nil {
		t.Fatal("Run() error = nil, want both start and on-stop errors")
	}
	if !strings.Contains(err.Error(), "start boom") || !strings.Contains(err.Error(), "onstop boom") {
		t.Fatalf("Run() error = %v, want both start boom and onstop boom", err)
	}
}

// TestRunRejectedTwice 回归：Run 不可重复调用——二次调用返回明确
// 错误，服务不会被二次 Start/Stop。
func TestRunRejectedTwice(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	c := &blockingService{name: "c"}
	app.Register(c)

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return c.started.Load() }, "Service to start")

	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("second Run() error = %v, want explicit once-only error", err)
	}

	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
}

// TestRegisterRacingRunLeavesNoOrphan 回归 Register/Run 并发裁决：Register
// 与 Run 并发时，迟到的注册必须在持锁登记事务内被裁决为 panic——不得留下
// "Init 成功、计入 healthCheckers、永不 Start/Stop" 的孤儿服务，也不得与
// run.Group 的 actors 产生 data race（-race 下运行）。
// 交错是确定性的：slow 的 Init 阻塞期间 Run 先置位 running，随后才放行。
func TestRegisterRacingRunLeavesNoOrphan(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	c1 := &blockingService{name: "c1"}
	slow := &slowInitService{
		name:        "slow",
		release:     make(chan struct{}),
		enteredInit: make(chan struct{}),
	}
	app.Register(c1)

	// 先发起 Register(slow)：fast-path running 检查在 Init 之前完成，
	// 随后进入阻塞的 Init。
	var recovered any
	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		defer func() { recovered = recover() }()
		app.Register(slow)
	}()
	<-slow.enteredInit

	// 再启动 Run：running 置位且 c1 已 Start 后，slow 的 Init 才被放行，
	// 其持锁登记事务必然命中 running 检查。
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return c1.started.Load() }, "c1 to start")
	close(slow.release)

	select {
	case <-regDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Register did not return after Init released")
	}
	if recovered == nil {
		t.Fatal("late Register racing Run did not panic")
	}
	if slow.started.Load() {
		t.Error("orphan Service was started")
	}
	if slow.stopped.Load() {
		t.Error("orphan Service was stopped")
	}

	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
}

// TestDrainChecker 验证框架内部排水检查器的状态语义。
func TestDrainChecker(t *testing.T) {
	var d drainChecker
	if err := d.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth() = %v, want nil before draining", err)
	}
	d.SetDraining(true)
	if err := d.CheckHealth(); err == nil {
		t.Fatal("CheckHealth() = nil, want error while draining")
	} else if !errors.Is(err, ErrDraining) {
		t.Fatalf("CheckHealth() = %v, want errors.Is(err, ErrDraining)", err)
	}
	d.SetDraining(false)
	if err := d.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth() = %v, want nil after draining cleared", err)
	}
}

// TestDrainTimeoutMarksReadinessUnhealthyBeforeShutdown 验证排水窗口语义：
// 关停信号（Close，与系统信号共用同一 shutdown 闭包）到达后，drainChecker
// 立即让 readiness 聚合（HealthCheckers()）失败，服务在 DrainTimeout
// 窗口内保持运行，窗口结束后才真正关停。
func TestDrainTimeoutMarksReadinessUnhealthyBeforeShutdown(t *testing.T) {
	const drain = 200 * time.Millisecond
	app, err := newLynx(NewOptions(WithDrainTimeout(drain)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	probe := &drainProbe{name: "probe"}
	app.Register(probe)

	// 启用排水时，框架内部 drainChecker 进入聚合：即使未注册任何 Checker
	// 服务，HealthCheckers() 也包含 1 个检查器，且平时健康。
	if got := len(app.HealthCheckers()); got != 1 {
		t.Fatalf("health checkers = %d, want 1 (drain checker registered)", got)
	}
	for _, c := range app.HealthCheckers() {
		if err := c.CheckHealth(); err != nil {
			t.Fatalf("readiness unhealthy before shutdown: %v", err)
		}
	}

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return probe.started.Load() }, "probe to start")

	closeAt := time.Now()
	app.Close()

	// 1) 排水窗口内：readiness 聚合立即不健康（LB 摘流生效）。
	waitFor(t, drain/2, func() bool {
		for _, c := range app.HealthCheckers() {
			if c.CheckHealth() != nil {
				return true
			}
		}
		return false
	}, "readiness to become unhealthy after Close")

	// 2) 同一时刻服务仍在运行：Start 尚未返回、Stop 尚未被调用。
	if probe.startReturn.Load() != 0 {
		t.Fatal("service Start returned before drain window elapsed")
	}
	if probe.stopped.Load() {
		t.Fatal("service stopped before drain window elapsed")
	}

	// 3) Run 返回发生在排水窗口之后：总关停时长 >= DrainTimeout。
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after drain window")
	}
	if elapsed := time.Since(closeAt); elapsed < drain {
		t.Errorf("shutdown elapsed %v, want >= drain timeout %v", elapsed, drain)
	}
	if probe.startReturn.Load() == 0 {
		t.Fatal("service Start never returned after shutdown")
	}
}

// TestDrainTimeoutZeroSkipsDrainWindow 回归：DrainTimeout=0（默认）时
// 关停路径与 v1.0 完全一致——不注册 drainChecker（HealthCheckers() 快照
// 内容一致），关停无排水延迟。快照内容的逐项断言另见
// TestHealthCheckersRegistered（默认选项下运行，即为 v1.0 回归证明）。
func TestDrainTimeoutZeroSkipsDrainWindow(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	if got := len(app.HealthCheckers()); got != 0 {
		t.Fatalf("health checkers = %d, want 0 (no drain checker when DrainTimeout=0)", got)
	}
	probe := &drainProbe{name: "probe"}
	app.Register(probe)
	if got := len(app.HealthCheckers()); got != 0 {
		t.Fatalf("health checkers = %d, want 0 (non-checker service must not be collected)", got)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return probe.started.Load() }, "probe to start")

	closeAt := time.Now()
	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
	// 无排水窗口：关停立即执行。若误加入 200ms 排水，此处必然失败。
	if elapsed := time.Since(closeAt); elapsed >= 200*time.Millisecond {
		t.Errorf("shutdown elapsed %v, want no drain delay (DrainTimeout=0)", elapsed)
	}
}

// TestOnDrainHooksRunBeforeOnStopWithinDrainWindow 验证 OnDrain 语义：
// 钩子在排水置位后与 DrainTimeout 睡眠并发执行，且在 OnStop 之前完成。
func TestOnDrainHooksRunBeforeOnStopWithinDrainWindow(t *testing.T) {
	const drain = 300 * time.Millisecond
	app, err := newLynx(NewOptions(WithDrainTimeout(drain)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	probe := &drainProbe{name: "probe"}
	app.Register(probe)

	drainRan := make(chan struct{})
	app.OnDrain(func(ctx context.Context) error {
		close(drainRan)
		return nil
	})
	// OnStop 执行时 OnDrain 必须已完成（shutdown 在 cancelCtx 前等待钩子收尾）。
	stopSawDrain := atomic.Bool{}
	app.OnStop(func(ctx context.Context) error {
		select {
		case <-drainRan:
			stopSawDrain.Store(true)
		default:
		}
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return probe.started.Load() }, "probe to start")

	closeAt := time.Now()
	app.Close()

	// 钩子与排水睡眠并发：排水窗口远未结束时钩子已经执行。
	select {
	case <-drainRan:
	case <-time.After(drain / 2):
		t.Fatal("on-drain hook did not run concurrently with drain window")
	}
	// 排水窗口本身仍然完整生效。
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
	if elapsed := time.Since(closeAt); elapsed < drain {
		t.Errorf("shutdown elapsed %v, want >= drain timeout %v", elapsed, drain)
	}
	if !stopSawDrain.Load() {
		t.Error("on-stop hook ran before on-drain hook completed")
	}
}

// TestOnDrainHookTimeoutDoesNotHangShutdown 验证阻塞的 OnDrain 钩子不会
// 挂死关停：超过 DrainHookTimeout 后记录错误并继续，Run 按时返回。
func TestOnDrainHookTimeoutDoesNotHangShutdown(t *testing.T) {
	const hookTimeout = 150 * time.Millisecond
	app, err := newLynx(NewOptions(WithDrainHookTimeout(hookTimeout)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	probe := &drainProbe{name: "probe"}
	app.Register(probe)

	// 忽略 ctx 的阻塞钩子：模拟挂死的注销逻辑。
	app.OnDrain(func(ctx context.Context) error {
		time.Sleep(10 * time.Second)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return probe.started.Load() }, "probe to start")

	closeAt := time.Now()
	app.Close()
	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), "on-drain hook timed out") {
			t.Fatalf("Run() error = %v, want it to contain %q", err, "on-drain hook timed out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
	// 上界由 DrainHookTimeout 决定，而非钩子的 10s 阻塞。
	if elapsed := time.Since(closeAt); elapsed >= 5*time.Second {
		t.Errorf("shutdown elapsed %v, want bounded by drain hook timeout %v", elapsed, hookTimeout)
	}
}

// TestOnDrainHookErrorSurfacesInRun 验证 OnDrain 钩子返回的错误随 Run() 上抛。
func TestOnDrainHookErrorSurfacesInRun(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	probe := &drainProbe{name: "probe"}
	app.Register(probe)

	wantErr := errors.New("deregister failed")
	app.OnDrain(func(ctx context.Context) error { return wantErr })

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return probe.started.Load() }, "probe to start")

	app.Close()
	select {
	case err := <-runErr:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run() error = %v, want errors.Is(err, %v)", err, wantErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
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
// 应用仍能启动（配置是可选的；默认 flags 开启后同样如此）。
func TestConfigFileNotFoundIsNotFatal(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx"}

	if _, err := newLynx(NewOptions()); err != nil {
		t.Fatalf("newLynx() error = %v, want nil when no config file is present", err)
	}
}

// TestExplicitConfigFileMissingIsFatal 验证显式指定的配置文件缺失时仍为硬错误。
func TestExplicitConfigFileMissingIsFatal(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-c", "does-not-exist.yaml"}

	if _, err := newLynx(NewOptions()); err == nil {
		t.Fatal("newLynx() error = nil, want error when explicit config file is missing")
	}
}

// TestUnknownFlagsIgnored 验证默认 flags 开启后，未知命令行参数（如 go test
// 二进制的 -test.*）不会导致初始化失败。
func TestUnknownFlagsIgnored(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-test.timeout=1m", "--unknown-flag=value"}

	if _, err := newLynx(NewOptions()); err != nil {
		t.Fatalf("newLynx() error = %v, want nil with unknown flags present", err)
	}
}

// TestDefaultFlagsDisabled 验证 WithDisableConfigFlags 关闭默认 flags 后，
// 命令行参数不再被解析。
func TestDefaultFlagsDisabled(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-test.timeout=1m"}

	o := NewOptions(WithDisableConfigFlags())
	if o.BindFlagsFunc != nil || o.BindConfigFunc != nil {
		t.Fatal("WithDisableConfigFlags should clear BindFlagsFunc and BindConfigFunc")
	}
	if _, err := newLynx(o); err != nil {
		t.Fatalf("newLynx() error = %v, want nil", err)
	}
}

// TestOnStartRunsBeforeServicesStart 验证 OnStart hooks 在服务启动前顺序执行。
func TestOnStartRunsBeforeServicesStart(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	c := &blockingService{name: "c1", record: rec.record}
	app.Register(c)
	app.OnStart(func(ctx context.Context) error {
		if c.started.Load() {
			t.Error("OnStart hook ran after Service had already started")
		}
		rec.record("onstart")
		return nil
	})

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	waitFor(t, 2*time.Second, func() bool { return c.started.Load() }, "Service to start")
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
		t.Fatalf("events = %v, want onstart recorded before Service start", events)
	}
}

// TestOnStopHookBlockingBoundedByTimeout 验证忽略 ctx 的阻塞 OnStop hook
// 不会挂起关闭流程，总时长受 ShutdownTimeout 约束。
func TestOnStopHookBlockingBoundedByTimeout(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	// 白盒收紧超时，避免 Options.Validate 的 MinTimeout 限制。
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

// TestRunnerSetupIsIdempotent 验证 setupApp() 只运行一次构建回调。
func TestRunnerSetupIsIdempotent(t *testing.T) {
	var calls atomic.Int32
	runner := NewRunner(func(app App) error {
		calls.Add(1)
		return nil
	})
	b, err := runner.setupApp()
	if err != nil {
		t.Fatal("setupApp() returned error")
	}
	if b == nil {
		t.Fatal("setupApp() returned nil")
	}
	b2, err := runner.setupApp()
	if err != nil {
		t.Fatal("second setupApp() returned error")
	}
	if b2 != b {
		t.Error("setupApp() should return the same app on subsequent calls")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("setup callback called %d times, want 1", got)
	}
}

// TestRunnerRunEReturnsInitError 验证 newLynx 初始化失败（如 Options 校验）
// 通过 RunE 返回而非直接退出进程。
func TestRunnerRunEReturnsInitError(t *testing.T) {
	runner := NewRunner(func(app App) error { return nil },
		WithName(strings.Repeat("a", 64))) // 触发 ErrNameTooLong
	if err := runner.RunE(); !errors.Is(err, ErrNameTooLong) {
		t.Fatalf("RunE() error = %v, want ErrNameTooLong", err)
	}
}

// TestCommandStartBeforeInit 验证未注册的 command 直接 Start 返回 ErrNotInitialized。
func TestCommandStartBeforeInit(t *testing.T) {
	cmd := NewCommand(func(ctx context.Context) error { return nil })
	if err := cmd.Start(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Start() error = %v, want ErrNotInitialized", err)
	}
}

// TestParseLogLevel 验证日志级别解析。
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
		lvl, err := ParseLogLevel(tt.in)
		if err != nil || lvl != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, %v; want %v", tt.in, lvl, err, tt.want)
		}
	}
	if _, err := ParseLogLevel("bogus"); err == nil {
		t.Error("ParseLogLevel(\"bogus\") should return an error")
	}
}

// TestLogLevelFromConfigPriority 验证日志级别键的优先级：
// logging.level → log-level → log_level。
func TestLogLevelFromConfigPriority(t *testing.T) {
	c := NewViperConfig(viper.New())
	if got := LogLevelFromConfig(c); got != "" {
		t.Errorf("LogLevelFromConfig() = %q, want empty", got)
	}
	c.Set("logging.level", "warn")
	c.Set("log-level", "error")
	c.Set("log_level", "debug")
	if got := LogLevelFromConfig(c); got != "warn" {
		t.Errorf("LogLevelFromConfig() = %q, want warn (logging.level wins)", got)
	}

	c = NewViperConfig(viper.New())
	c.Set("log-level", "error")
	c.Set("log_level", "debug")
	if got := LogLevelFromConfig(c); got != "error" {
		t.Errorf("LogLevelFromConfig() = %q, want error (log-level beats log_level)", got)
	}

	c = NewViperConfig(viper.New())
	c.Set("log_level", "debug")
	if got := LogLevelFromConfig(c); got != "debug" {
		t.Errorf("LogLevelFromConfig() = %q, want debug (log_level fallback)", got)
	}
}

// TestLogLevelConfigNotShadowedByUnchangedFlag 回归：--log-level 未显式传入
// 时，配置文件的日志级别键必须生效（flag 默认值为空，不得遮蔽配置键）。
func TestLogLevelConfigNotShadowedByUnchangedFlag(t *testing.T) {
	v := viper.New()
	v.Set("log_level", "debug")

	f := pflag.NewFlagSet("test", pflag.ContinueOnError)
	DefaultBindFlagsFunc(f) // 未 Parse 任何参数：所有 flag 均未 Changed
	if err := v.BindPFlags(f); err != nil {
		t.Fatalf("BindPFlags: %v", err)
	}
	if got := LogLevelFromConfig(NewViperConfig(v)); got != "debug" {
		t.Errorf("LogLevelFromConfig() = %q, want debug（未传的 --log-level 不应遮蔽配置文件）", got)
	}

	// 显式传入时 flag 优先于配置键。
	f2 := pflag.NewFlagSet("test2", pflag.ContinueOnError)
	DefaultBindFlagsFunc(f2)
	if err := f2.Parse([]string{"--log-level=warn"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := v.BindPFlags(f2); err != nil {
		t.Fatalf("BindPFlags: %v", err)
	}
	if got := LogLevelFromConfig(NewViperConfig(v)); got != "warn" {
		t.Errorf("LogLevelFromConfig() = %q, want warn（显式 flag 优先）", got)
	}
}

// TestAppContextCarriesServiceConfigKeys 验证 service.name/service.id/
// service.version 配置键写入应用上下文。
func TestAppContextCarriesServiceConfigKeys(t *testing.T) {
	v := viper.New()
	v.Set("service.name", "svc-new")
	v.Set("service.id", "id-new")
	v.Set("service.version", "v-new")
	c := NewViperConfig(v)

	app, err := newLynxWithConfig(c)
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	ctx := app.Context()
	want := Metadata{Name: "svc-new", ID: "id-new", Version: "v-new"}
	if got := Meta(ctx); got != want {
		t.Errorf("Meta() = %+v, want %+v", got, want)
	}
}

// TestAppContextIgnoresLegacyTopLevelKeys 验证旧顶层 name/id/version 键
// 自 v1.0 起不再生效（已移除回退），元数据一律取 service.* 键或 Options。
func TestAppContextIgnoresLegacyTopLevelKeys(t *testing.T) {
	v := viper.New()
	v.Set("name", "svc-old")
	v.Set("id", "id-old")
	v.Set("version", "v-old")
	c := NewViperConfig(v)

	app, err := newLynxWithConfig(c)
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	ctx := app.Context()
	got := Meta(ctx)
	if got.Name != DefaultName {
		t.Errorf("Meta().Name = %q, want Options 默认值 %q（旧键已失效）", got.Name, DefaultName)
	}
	if got.ID == "id-old" {
		t.Errorf("Meta().ID = %q, 旧顶层键不应生效", got.ID)
	}
	if got.Version == "v-old" {
		t.Errorf("Meta().Version = %q, 旧顶层键不应生效", got.Version)
	}
}

// newLynxWithConfig 用预置配置源构建应用（绕过命令行 flags 解析）。
func newLynxWithConfig(c ConfigSource) (App, error) {
	o := NewOptions()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	f := pflag.NewFlagSet(os.Args[0], pflag.ContinueOnError)
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
	app.services = []Service{}
	app.c = c.(*viperConfig).v
	if err := app.init(); err != nil {
		return nil, err
	}
	return app, nil
}
