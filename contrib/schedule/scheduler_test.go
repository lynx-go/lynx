package schedule

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(ctx)
	}()

	if !pollUntil(3*time.Second, 10*time.Millisecond, func() bool { return count.Load() >= 1 }) {
		_ = s.Stop(context.Background())
		cancel()
		t.Fatalf("task did not run within 3s")
	}
	_ = s.Stop(context.Background())
	// 对齐框架顺序：Stop 返回后取消 Start 的 ctx，Start 随即退出。
	cancel()

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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(ctx)
	}()

	// The handler panics on every tick; the scheduler must survive and keep
	// invoking the task on subsequent ticks.
	if !pollUntil(4*time.Second, 10*time.Millisecond, func() bool { return count.Load() >= 2 }) {
		_ = s.Stop(context.Background())
		cancel()
		t.Fatalf("scheduler did not keep running after handler panic (count=%d)", count.Load())
	}
	_ = s.Stop(context.Background())
	cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(ctx)
	}()

	// While running: must not error.
	if !pollUntil(2*time.Second, 5*time.Millisecond, func() bool { return s.CheckHealth() == nil }) {
		_ = s.Stop(context.Background())
		cancel()
		t.Fatalf("expected CheckHealth to succeed while running")
	}

	_ = s.Stop(context.Background())
	cancel()
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Start(ctx) }()

	// First run blocks for 500ms; subsequent ticks must be skipped, not
	// started concurrently.
	time.Sleep(1200 * time.Millisecond)
	_ = s.Stop(context.Background())
	cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(ctx)
	}()

	if !pollUntil(4*time.Second, 10*time.Millisecond, func() bool { return count.Load() >= 2 }) {
		_ = s.Stop(context.Background())
		cancel()
		t.Fatalf("scheduler did not keep running after handler error (count=%d)", count.Load())
	}
	_ = s.Stop(context.Background())
	cancel()
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
		_ = s.Stop(context.Background())
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

// TestStopRacingStartNoHang 回归：Stop 与 Start 并发交错时不得挂死。
// 复现窗口：Start 通过 stopping 检查 → Stop 读到 started==false 提前返回
// → Start 启动 cron 后阻塞在 <-ctx.Done()。新语义下 Start 尊重传入的 ctx
// （框架在 Stop 返回后取消服务 ctx），测试以 cancel 解除等待；stopping
// 标志的握手不得在任何交错过挂死。10 万次交错中任一次挂死即失败。
func TestStopRacingStartNoHang(t *testing.T) {
	for i := 0; i < 100_000; i++ {
		s, err := NewScheduler(nil, WithLogger(discardLogger()))
		if err != nil {
			t.Fatalf("iteration %d: NewScheduler: %v", i, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		go func() {
			defer close(started)
			_ = s.Start(ctx)
		}()
		_ = s.Stop(context.Background())
		cancel()
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
		_ = s.Stop(context.Background())
		t.Fatal("error handler was not invoked")
	}
	cancel()
	_ = s.Stop(context.Background())
	if name, _ := gotName.Load().(string); name != "failing" {
		t.Fatalf("error handler task name = %q, want failing", name)
	}
}

// TestStartRespectsCtx 回归：Start 必须阻塞在传入 ctx 上，ctx 取消即返回
// （run.Group actor 语义）。
func TestStartRespectsCtx(t *testing.T) {
	var count atomic.Int32
	s, err := NewScheduler(
		[]Task{newCountingTask("t1", "@every 1h", &count)},
		WithLogger(discardLogger()),
	)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Start returned prematurely: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after ctx cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}

// fakeAppCtx 是 lynx.AppContext 的最小测试替身（Init 注入测试用）。
type fakeAppCtx struct {
	logger *slog.Logger
}

func (f *fakeAppCtx) Context() context.Context       { return context.Background() }
func (f *fakeAppCtx) Config() lynx.Config            { return nil }
func (f *fakeAppCtx) Bus() eventbus.Bus              { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (f *fakeAppCtx) Logger(...any) *slog.Logger     { return f.logger }
func (f *fakeAppCtx) HealthCheckers() []lynx.Checker { return nil }
func (f *fakeAppCtx) Close()                         {}

// TestInitKeepsExplicitLogger 回归 AUX-02：WithLogger 显式设置的实例不被
// Init 的 ctx.Logger 覆盖（对齐 debug 包的 loggerSet 防护）。
func TestInitKeepsExplicitLogger(t *testing.T) {
	explicit := discardLogger()
	s, err := NewScheduler(nil, WithLogger(explicit))
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Init(&fakeAppCtx{logger: slog.Default()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if s.logger != explicit {
		t.Fatal("Init should not override explicit WithLogger instance")
	}
}

// TestInitWithoutExplicitLoggerUsesCtxLogger 验证未显式 WithLogger 时 Init
// 仍取 ctx.Logger（loggerSet 防护不改变默认行为）。
func TestInitWithoutExplicitLoggerUsesCtxLogger(t *testing.T) {
	ctxLogger := discardLogger()
	s, err := NewScheduler(nil)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Init(&fakeAppCtx{logger: ctxLogger}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if s.logger != ctxLogger {
		t.Fatal("Init should use ctx.Logger when WithLogger not set")
	}
}

// TestStopWaitsForInflightTask 回归 AUX-06：Stop 必须等待 cron 在途任务
// 收敛（cron.Stop 返回的等待句柄 Done），而非只等 Start 协程退出或一路
// 阻塞到 deadline 耗尽。
func TestStopWaitsForInflightTask(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var finished atomic.Bool
	task := &testTask{
		name: "inflight",
		// 高频触发：首个 tick 即产生一个由 cron 调度发起的在途任务
		//（必须由 cron 发起而非测试手动 Run，否则不进入其 jobWaiter
		// 追踪，Stop 的等待句柄不会为它等待）。
		cron: "@every 100ms",
		handler: func(ctx context.Context) error {
			startedOnce.Do(func() { close(started) })
			<-release
			finished.Store(true)
			return nil
		},
	}
	s, err := NewScheduler([]Task{task}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan struct{})
	go func() { defer close(startDone); _ = s.Start(ctx) }()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("inflight task did not start")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	stopDone := make(chan struct{})
	go func() { _ = s.Stop(stopCtx); close(stopDone) }()

	// 任务仍阻塞时 Stop 不得返回：旧行为下 select 只等 runDone/ctx.Done
	//（runDone 因 Start ctx 未取消而不会关闭），只会干等 deadline 耗尽。
	select {
	case <-stopDone:
		cancel()
		t.Fatal("Stop returned while inflight task still running")
	case <-time.After(200 * time.Millisecond):
	}

	// 释放任务后 Stop 应随收敛立即返回（远早于 5s deadline）。
	close(release)
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Stop did not return after inflight task finished")
	}
	if !finished.Load() {
		t.Fatal("inflight task reported finished=false after Stop returned")
	}
	cancel()
	<-startDone
}

// bufferLogger 返回写入 buffer 的 slog 实例，供日志内容断言。
func bufferLogger() (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	return buf, slog.New(slog.NewTextHandler(buf, nil))
}

// TestWithLocationWarnOnCustomCron 回归 AUX-13：WithLocation 对 WithCron
// 自定义实例静默失效，必须留下 Warn 日志指引。
func TestWithLocationWarnOnCustomCron(t *testing.T) {
	buf, logger := bufferLogger()
	_, err := NewScheduler(nil,
		WithLogger(logger),
		WithCron(cron.New()),
		WithLocation(time.UTC),
	)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WithLocation") || !strings.Contains(out, "ignored") {
		t.Errorf("missing WithLocation-ignored warning, got: %s", out)
	}
}

// TestWithLocationSilentOnDefaultCron 验证默认 cron 路径不误报 Warn。
func TestWithLocationSilentOnDefaultCron(t *testing.T) {
	buf, logger := bufferLogger()
	_, err := NewScheduler(nil, WithLogger(logger), WithLocation(time.UTC))
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "WithLocation") {
		t.Errorf("unexpected warning for default cron instance, got: %s", out)
	}
}

// TestWithLocationAffectsScheduleTime 覆盖 AUX-17 时区测试缺口：断言
// 内置 cron 实例按指定时区计算下次触发时间（Entry.Next 的墙上时钟为
// 该时区的 09:00:00），而非宿主机本地时区。
func TestWithLocationAffectsScheduleTime(t *testing.T) {
	// 选 UTC+7 这类非整点无偏移的时区：与多数测试机本地时区（UTC+8 或
	// 其他）区分，本地时区恰好也是 UTC+7 时仍可用 UTC 对照排除。
	loc := time.FixedZone("test-utc-plus-7", 7*3600)
	s, err := NewScheduler(
		[]Task{newCountingTask("tz", "0 0 9 * * *", &atomic.Int32{})},
		WithLogger(discardLogger()),
		WithLocation(loc),
	)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := s.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Start(ctx) }()

	// Entry.Next 由调度循环计算，启动后短暂轮询等待填充。
	if !pollUntil(2*time.Second, 5*time.Millisecond, func() bool {
		return len(s.cron.Entries()) > 0 && !s.cron.Entries()[0].Next.IsZero()
	}) {
		t.Fatal("cron entry Next was not computed")
	}
	next := s.cron.Entries()[0].Next
	if h, m, sec := next.In(loc).Clock(); h != 9 || m != 0 || sec != 0 {
		t.Errorf("next fire time = %v (in %s), want 09:00:00 in %s", next.In(loc), loc, loc)
	}

	cancel()
	<-done
}
