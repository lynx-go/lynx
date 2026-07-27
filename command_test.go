package lynx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocloud.dev/server/health"
)

// sequenceChecker fails CheckHealth for the first `failures` calls, then succeeds.
type sequenceChecker struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (c *sequenceChecker) CheckHealth() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= c.failures {
		return errors.New("not ready")
	}
	return nil
}

func (c *sequenceChecker) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newAppWithCheckers(t *testing.T, checkers ...health.Checker) Lynx {
	t.Helper()
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.(*lynx).healthCheckers = checkers
	return app
}

func TestNewCommandDefaults(t *testing.T) {
	cmd := NewCommand(func(ctx context.Context) error { return nil })
	c, ok := cmd.(*command)
	if !ok {
		t.Fatalf("NewCommand() type = %T, want *command", cmd)
	}
	if c.options.MaxTries != 10 {
		t.Errorf("MaxTries = %d, want 10", c.options.MaxTries)
	}
	if c.options.InitialBackoff != 100*time.Millisecond {
		t.Errorf("InitialBackoff = %v, want 100ms", c.options.InitialBackoff)
	}
	if c.options.MaxBackoff != 30*time.Second {
		t.Errorf("MaxBackoff = %v, want 30s", c.options.MaxBackoff)
	}
	if c.Name() != "command" {
		t.Errorf("Name() = %q, want %q", c.Name(), "command")
	}
}

func TestNewCommandOptions(t *testing.T) {
	cmd := NewCommand(nil, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	c := cmd.(*command)
	if c.options.MaxTries != 3 {
		t.Errorf("MaxTries = %d, want 3", c.options.MaxTries)
	}
	if c.options.InitialBackoff != time.Millisecond {
		t.Errorf("InitialBackoff = %v, want 1ms", c.options.InitialBackoff)
	}
	if c.options.MaxBackoff != 5*time.Millisecond {
		t.Errorf("MaxBackoff = %v, want 5ms", c.options.MaxBackoff)
	}
}

func TestCommandStartHealthy(t *testing.T) {
	checker := &sequenceChecker{}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	if got := checker.Calls(); got != 1 {
		t.Errorf("health checked %d times, want 1", got)
	}
}

func TestCommandStartRetriesUntilHealthy(t *testing.T) {
	checker := &sequenceChecker{failures: 2}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(5), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	start := time.Now()
	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	if got := checker.Calls(); got != 3 {
		t.Errorf("health checked %d times, want 3 (2 failures + 1 success)", got)
	}
	// Two retries with ~1ms initial backoff: must be well under a second.
	if elapsed > 2*time.Second {
		t.Errorf("Start() took %v, backoff seems not to honor WithBackoff", elapsed)
	}
}

func TestCommandStartExhaustsRetries(t *testing.T) {
	checker := &sequenceChecker{failures: 100}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := cmd.Start(context.Background())
	if err == nil {
		t.Fatal("Start() error = nil, want retry exhaustion error")
	}
	if !strings.Contains(err.Error(), "failed to start components") {
		t.Errorf("Start() error = %v, want it to mention %q", err, "failed to start components")
	}
	if got := checker.Calls(); got != 3 {
		t.Errorf("health checked %d times, want 3 (MaxTries)", got)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
}

func TestCommandStartContextCancelled(t *testing.T) {
	// With an always-failing checker and a cancelled context, the retry loop
	// must abort instead of exhausting MaxTries.
	checker := &sequenceChecker{failures: 100}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(10), WithBackoff(time.Second, 5*time.Second))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := cmd.Start(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Start() error = nil, want error for cancelled context")
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
	// Must abort on the cancelled context rather than retrying with 1s backoff.
	if elapsed > time.Second {
		t.Errorf("Start() took %v, want immediate abort on cancelled context", elapsed)
	}
	if got := checker.Calls(); got >= 10 {
		t.Errorf("health checked %d times, want fewer than MaxTries on cancelled context", got)
	}
}

func TestCommandStartHealthyWithCancelledContext(t *testing.T) {
	// A healthy checker succeeds on the first attempt, so the cancelled
	// context is never consulted by the retry loop and the command runs.
	checker := &sequenceChecker{}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := cmd.Start(ctx); err != nil {
		t.Errorf("Start() error = %v, want nil for healthy checker", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
}

func TestCommandFnErrorPropagates(t *testing.T) {
	app := newAppWithCheckers(t)

	wantErr := errors.New("command failed")
	cmd := NewCommand(func(ctx context.Context) error {
		return wantErr
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := cmd.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want %v", err, wantErr)
	}
}

func TestCommandStopClosesApp(t *testing.T) {
	app := newAppWithCheckers(t)
	cmd := NewCommand(func(ctx context.Context) error { return nil })
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	cmd.Stop(context.Background())

	select {
	case <-app.Context().Done():
	case <-time.After(time.Second):
		t.Error("Stop() should close the app")
	}
}
