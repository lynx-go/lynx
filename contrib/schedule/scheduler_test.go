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

// TestStopBeforeStartNoHang 回归 M2：Stop 先于 Start 调用时，cron.Stop()
// 空转（robfig/cron 未 running 时不发停止信号），Stop 不得挂起，且随后
// Start 不得启动无人能停的无限循环。
func TestStopBeforeStartNoHang(t *testing.T) {
	var count atomic.Int32
	s, err := NewScheduler(
		[]Task{newCountingTask("t1", "@every 100ms", &count)},
		WithLogger(discardLogger()),
	)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		s.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung when called before Start")
	}

	// Start 随后调用必须立即返回（不启动 cron 无限循环）。
	started := make(chan struct{})
	go func() {
		_ = s.Start(context.Background())
		close(started)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Start hung when called after Stop")
	}

	// 给足一个调度周期：任务必须从未执行。
	time.Sleep(300 * time.Millisecond)
	if n := count.Load(); n != 0 {
		t.Fatalf("task executed %d times after Stop-before-Start, want 0", n)
	}
}

// TestStopRacingStartNoHang 回归 P0-3：Stop 与 Start 并发交错时不得挂死。
// 复现窗口：Start 通过 stopping 检查 → Stop 读到 cancel==nil 且 started==false
// 提前返回 → Start 创建 Background 衍生 ctx 后阻塞在 <-s.ctx.Done()，
// 该 ctx 永无人取消。10 万次交错中任一次挂死即失败。
func TestStopRacingStartNoHang(t *testing.T) {
	for i := 0; i < 100_000; i++ {
		s, err := NewScheduler(nil, WithLogger(discardLogger()))
		if err != nil {
			t.Fatalf("iteration %d: NewScheduler: %v", i, err)
		}
		started := make(chan struct{})
		go func() {
			defer close(started)
			_ = s.Start(context.Background())
		}()
		s.Stop(context.Background())
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Start hung after racing Stop", i)
		}
	}
}

// TestErrorHandlerInvoked 验证 WithErrorHandler 回调接收任务错误。
func TestErrorHandlerInvoked(t *testing.T) {
	var got atomic.Int32
	var gotName atomic.Value
	s, err := NewScheduler(
		[]Task{&testTask{name: "failing", cron: "@every 50ms", handler: func(ctx context.Context) error {
			return errors.New("boom")
		}}},
		WithLogger(discardLogger()),
		WithErrorHandler(func(ctx context.Context, task Task, err error) {
			got.Add(1)
			gotName.Store(task.Name())
		}),
	)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Start(ctx) }()
	if !pollUntil(2*time.Second, 10*time.Millisecond, func() bool { return got.Load() > 0 }) {
		cancel()
		s.Stop(context.Background())
		t.Fatal("error handler was not invoked")
	}
	cancel()
	s.Stop(context.Background())
	if name, _ := gotName.Load().(string); name != "failing" {
		t.Fatalf("error handler task name = %q, want failing", name)
	}
}
