package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// testTask is a minimal Task implementation for tests.
type testTask struct {
	name    string
	cron    string
	handler HandlerFunc
}

func (t *testTask) Name() string             { return t.name }
func (t *testTask) Cron() string             { return t.cron }
func (t *testTask) HandlerFunc() HandlerFunc { return t.handler }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newCountingTask(name, spec string, count *atomic.Int32) *testTask {
	return &testTask{
		name: name,
		cron: spec,
		handler: func(ctx context.Context) error {
			count.Add(1)
			return nil
		},
	}
}

// pollUntil polls cond every interval until it returns true or the deadline
// expires. Returns the final value of cond.
func pollUntil(deadline time.Duration, interval time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(interval)
	}
	return cond()
}

func TestNewScheduler(t *testing.T) {
	tests := []struct {
		name    string
		tasks   []Task
		wantErr bool
	}{
		{
			name:    "no tasks",
			tasks:   nil,
			wantErr: false,
		},
		{
			name: "valid tasks",
			tasks: []Task{
				newCountingTask("t1", "@every 1s", &atomic.Int32{}),
				newCountingTask("t2", "0 0 * * * *", &atomic.Int32{}),
			},
			wantErr: false,
		},
		{
			name: "invalid cron expression",
			tasks: []Task{
				newCountingTask("bad", "not-a-cron-expression", &atomic.Int32{}),
			},
			wantErr: true,
		},
		{
			name: "5-field spec rejected when seconds are enabled",
			tasks: []Task{
				newCountingTask("bad", "* * * * *", &atomic.Int32{}),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewScheduler(tt.tasks, WithLogger(discardLogger()))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if s != nil {
					t.Fatalf("expected nil scheduler on error, got %v", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s == nil {
				t.Fatalf("expected scheduler, got nil")
			}
			if got := len(s.cron.Entries()); got != len(tt.tasks) {
				t.Fatalf("expected %d cron entries, got %d", len(tt.tasks), got)
			}
		})
	}
}

// TestNewSchedulerRegistersTasksOnce is a regression test for the fix in
// commit 31e1db2: tasks were previously registered twice (once in
// NewScheduler, once in Init), causing every task to run twice.
func TestNewSchedulerRegistersTasksOnce(t *testing.T) {
	tasks := []Task{
		newCountingTask("t1", "@every 1s", &atomic.Int32{}),
		newCountingTask("t2", "@every 1s", &atomic.Int32{}),
		newCountingTask("t3", "@every 1s", &atomic.Int32{}),
	}
	s, err := NewScheduler(tasks, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(s.cron.Entries()); got != len(tasks) {
		t.Fatalf("after NewScheduler: expected %d cron entries, got %d", len(tasks), got)
	}

	// Init must NOT register the tasks again.
	if err := s.Init(nil); err != nil {
		t.Fatalf("unexpected Init error: %v", err)
	}
	if got := len(s.cron.Entries()); got != len(tasks) {
		t.Fatalf("after Init: expected %d cron entries, got %d (double registration regression)", len(tasks), got)
	}
}

func TestSchedulerOptions(t *testing.T) {
	customCron := cron.New()
	logger := discardLogger()

	s, err := NewScheduler(nil,
		WithLogger(logger),
		WithCron(customCron),
		WithDebugEnabled(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.options.Logger != logger {
		t.Errorf("WithLogger not applied")
	}
	if s.options.Cron != customCron {
		t.Errorf("WithCron not applied")
	}
	if !s.options.DebugEnabled {
		t.Errorf("WithDebugEnabled not applied")
	}
	if s.cron != customCron {
		t.Errorf("scheduler should use the cron instance provided via WithCron")
	}
	if got := s.Name(); got != "cron-scheduler" {
		t.Errorf("unexpected Name(): %q", got)
	}
}

func TestSchedulerDefaultOptions(t *testing.T) {
	s, err := NewScheduler(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.options.Logger == nil {
		t.Errorf("expected default logger to be set")
	}
	if s.cron == nil {
		t.Errorf("expected a default cron instance to be created")
	}
	if s.options.DebugEnabled {
		t.Errorf("DebugEnabled should default to false")
	}
}

func TestSchedulerRunsTask(t *testing.T) {
	var count atomic.Int32
	task := newCountingTask("counter", "@every 1s", &count)

	s, err := NewScheduler([]Task{task}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(context.Background())
	}()

	if !pollUntil(3*time.Second, 10*time.Millisecond, func() bool { return count.Load() >= 1 }) {
		s.Stop(context.Background())
		t.Fatalf("task did not run within 3s")
	}
	s.Stop(context.Background())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Start did not return after Stop")
	}
}

func TestSchedulerRecoversFromPanic(t *testing.T) {
	var count atomic.Int32
	task := &testTask{
		name: "panicking",
		cron: "@every 1s",
		handler: func(ctx context.Context) error {
			count.Add(1)
			panic("boom")
		},
	}

	s, err := NewScheduler([]Task{task}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(context.Background())
	}()

	// The handler panics on every tick; the scheduler must survive and keep
	// invoking the task on subsequent ticks.
	if !pollUntil(4*time.Second, 10*time.Millisecond, func() bool { return count.Load() >= 2 }) {
		s.Stop(context.Background())
		t.Fatalf("scheduler did not keep running after handler panic (count=%d)", count.Load())
	}
	s.Stop(context.Background())
	<-done
}

// TestSchedulerCheckHealthLifecycle locks the behavior: CheckHealth must
// error before Start (not running) and after Stop.
func TestSchedulerCheckHealthLifecycle(t *testing.T) {
	s, err := NewScheduler(nil, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Before Start: must error.
	if err := s.CheckHealth(); err == nil {
		t.Fatalf("expected CheckHealth error before Start")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(context.Background())
	}()

	// While running: must not error.
	if !pollUntil(2*time.Second, 5*time.Millisecond, func() bool { return s.CheckHealth() == nil }) {
		s.Stop(context.Background())
		t.Fatalf("expected CheckHealth to succeed while running")
	}

	s.Stop(context.Background())
	<-done

	// After Stop: must error again.
	if err := s.CheckHealth(); err == nil {
		t.Fatalf("expected CheckHealth error after Stop")
	}
}

// TestSchedulerSkipsOverlappingRuns verifies the default SkipIfStillRunning
// chain: a task that takes longer than its interval must not run concurrently.
func TestSchedulerSkipsOverlappingRuns(t *testing.T) {
	var running atomic.Int32
	var maxRunning atomic.Int32
	task := &testTask{
		name: "slow",
		cron: "@every 100ms",
		handler: func(ctx context.Context) error {
			cur := running.Add(1)
			for {
				old := maxRunning.Load()
				if cur <= old || maxRunning.CompareAndSwap(old, cur) {
					break
				}
			}
			defer running.Add(-1)
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
			return nil
		},
	}

	s, err := NewScheduler([]Task{task}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Start(context.Background()) }()

	// First run blocks for 500ms; subsequent ticks must be skipped, not
	// started concurrently.
	time.Sleep(1200 * time.Millisecond)
	s.Stop(context.Background())
	<-done

	if got := maxRunning.Load(); got > 1 {
		t.Errorf("max concurrent executions = %d, want 1 (overlapping runs not skipped)", got)
	}
}

func TestSchedulerHandlerErrorDoesNotStopScheduler(t *testing.T) {
	var count atomic.Int32
	task := &testTask{
		name: "failing",
		cron: "@every 1s",
		handler: func(ctx context.Context) error {
			count.Add(1)
			return errors.New("task failed")
		},
	}

	s, err := NewScheduler([]Task{task}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(context.Background())
	}()

	if !pollUntil(4*time.Second, 10*time.Millisecond, func() bool { return count.Load() >= 2 }) {
		s.Stop(context.Background())
		t.Fatalf("scheduler did not keep running after handler error (count=%d)", count.Load())
	}
	s.Stop(context.Background())
	<-done
}
