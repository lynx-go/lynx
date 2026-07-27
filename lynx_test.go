package lynx

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func (c *blockingComponent) Init(app Lynx) error { return nil }

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
func (c *failInitComponent) Init(app Lynx) error             { return c.err }
func (c *failInitComponent) Start(ctx context.Context) error { return nil }
func (c *failInitComponent) Stop(ctx context.Context)        {}

// failStartComponent fails in Start.
type failStartComponent struct {
	name string
	err  error
}

func (c *failStartComponent) Name() string                    { return c.name }
func (c *failStartComponent) Init(app Lynx) error             { return nil }
func (c *failStartComponent) Start(ctx context.Context) error { return c.err }
func (c *failStartComponent) Stop(ctx context.Context)        {}

// checkerComponent implements both Component and health.Checker.
type checkerComponent struct {
	HealthChecker
	name string
}

func (c *checkerComponent) Name() string        { return c.name }
func (c *checkerComponent) Init(app Lynx) error { return nil }
func (c *checkerComponent) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (c *checkerComponent) Stop(ctx context.Context) {}

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

func TestHooksComponentInitError(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	wantErr := errors.New("init failed")
	err = app.Hooks(Components(&failInitComponent{name: "bad", err: wantErr}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Hooks() error = %v, want %v", err, wantErr)
	}
}

func TestComponentBuildersInstances(t *testing.T) {
	tests := []struct {
		name       string
		instances  int
		wantBuilds int32
	}{
		{name: "zero instances defaults to one", instances: 0, wantBuilds: 1},
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
			if err := app.Hooks(ComponentBuilders(builder)); err != nil {
				t.Fatalf("Hooks() error = %v", err)
			}
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
	if err := app.Hooks(Components(comp)); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}

	checkers := app.HealthCheckFunc()()
	if len(checkers) != 1 {
		t.Fatalf("health checkers = %d, want 1", len(checkers))
	}
	if checkers[0] != comp {
		t.Error("registered health checker is not the component")
	}

	// A component that is not a health.Checker must not be registered.
	if err := app.Hooks(Components(&blockingComponent{name: "plain"})); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}
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
	if err := app.Hooks(Components(c1, c2)); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}
	if err := app.Hooks(OnStop(func(ctx context.Context) error {
		rec.record("onstop")
		return nil
	})); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}

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

	// Stop ordering is deterministic: components stop in registration order,
	// then OnStop hooks run during shutdown.
	var stops []string
	for _, e := range events {
		if e == "stop:c1" || e == "stop:c2" || e == "onstop" {
			stops = append(stops, e)
		}
	}
	want := []string{"stop:c1", "stop:c2", "onstop"}
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
	if err := app.Hooks(OnStart(func(ctx context.Context) error {
		return wantErr
	})); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}

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
	if err := app.Hooks(Components(&failStartComponent{name: "bad", err: wantErr})); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}

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
	if err := app.Hooks(OnStop(record("hook1"), record("hook2"), record("hook3"))); err != nil {
		t.Fatalf("Hooks() error = %v", err)
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	// Give Run a moment to register its actors, then shut down.
	time.Sleep(50 * time.Millisecond)
	app.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
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
