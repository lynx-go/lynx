package lynx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// recordingBus 记录 Stop 调用顺序，用于断言 post-stop 钩子在总线关停之后执行。
type recordingBus struct {
	readyGateBus
	mu      sync.Mutex
	stopped bool
}

func (b *recordingBus) Name() string { return "recording-bus" }
func (b *recordingBus) Stop(ctx context.Context) error {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	return nil
}
func (b *recordingBus) isStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

// TestOnPostStartRunsAfterPreStartAndDuringRunning 验证 OnPostStart 的
// 触发契约：晚于 OnPreStart（服务派发前）执行，且在应用运行期间触发
// （无需等待关停）。触发界是"所有服务 actor 已进入执行体"（startWG
// 归零）；阻塞型服务的 Start 体内副作用与钩子最初几条指令可能并发，
// 故不断言钩子内可见服务 Start 体内的副作用（运行通知语义，非就绪）。
func TestOnPostStartRunsAfterPreStartAndDuringRunning(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	c1 := &blockingService{name: "c1"}
	c2 := &blockingService{name: "c2"}
	app.Register(c1, c2)
	app.OnPreStart(func(ctx context.Context) error {
		if c1.started.Load() || c2.started.Load() {
			t.Error("OnPreStart hook ran after Service had already started")
		}
		rec.record("prestart")
		return nil
	})
	app.OnPostStart(func(ctx context.Context) error {
		rec.record("poststart")
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()

	// 强断言：post-start 在应用运行期间触发（此时未发起任何关停）。
	waitFor(t, 2*time.Second, func() bool {
		for _, e := range rec.snapshot() {
			if e == "poststart" {
				return true
			}
		}
		return false
	}, "post-start hook to run while app is running")

	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}

	// 顺序断言：poststart 晚于 prestart（run group 派发前）。
	events := rec.snapshot()
	var prestartIdx, poststartIdx int
	poststartIdx = -1
	for i, e := range events {
		if e == "prestart" {
			prestartIdx = i
		}
		if e == "poststart" {
			poststartIdx = i
		}
	}
	if poststartIdx == -1 || poststartIdx < prestartIdx {
		t.Fatalf("events = %v, want poststart after prestart", events)
	}
}

// TestOnPostStartWithZeroServices 验证无服务应用同样触发 OnPostStart。
func TestOnPostStartWithZeroServices(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	ran := make(chan struct{})
	app.OnPostStart(func(ctx context.Context) error {
		close(ran)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("OnPostStart did not run for app without services")
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

// TestOnPostStartHookErrorTriggersShutdown 验证 post-start 钩子错误视为
// 启动失败：触发关停（服务被 Stop、post-stop 钩子仍执行）并随 Run() 上抛。
func TestOnPostStartHookErrorTriggersShutdown(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	stopped := make(chan string, 1)
	app.Register(&stopRecorder{name: "svc", stopped: stopped})

	postStopRan := make(chan struct{})
	wantErr := errors.New("poststart boom")
	app.OnPostStart(func(ctx context.Context) error { return wantErr })
	app.OnPostStop(func() {
		close(postStopRan)
	})

	err = app.Run()
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
	select {
	case name := <-stopped:
		if name != "svc" {
			t.Fatalf("stopped %q, want svc", name)
		}
	case <-time.After(time.Second):
		t.Fatal("Service was not stopped after post-start hook failure")
	}
	select {
	case <-postStopRan:
	case <-time.After(time.Second):
		t.Fatal("post-stop hook did not run after post-start hook failure")
	}
}

// TestOnPostStopRunsAfterServicesAndBusStopped 验证 post-stop 钩子的执行
// 位置：OnPreStop → 服务 Stop → 总线 Stop → post-stop。
func TestOnPostStopRunsAfterServicesAndBusStopped(t *testing.T) {
	bus := &recordingBus{}
	app, err := newLynx(NewOptions(WithBus(bus)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	svc := &blockingService{name: "svc", record: rec.record}
	app.Register(svc)
	app.OnPreStop(func(ctx context.Context) error {
		if svc.started.Load() {
			rec.record("prestop-while-serving")
		}
		return nil
	})
	app.OnPostStop(func() {
		// post-stop 执行时：服务已 Stop，总线已 Stop。
		rec.record("poststop")
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitFor(t, 2*time.Second, func() bool { return svc.started.Load() }, "Service to start")

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
	want := []string{"start:svc", "prestop-while-serving", "stop:svc", "poststop"}
	var got []string
	for _, e := range events {
		if e == "prestop-while-serving" || e == "start:svc" || e == "stop:svc" || e == "poststop" {
			got = append(got, e)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if !bus.isStopped() {
		t.Fatal("bus was not stopped before post-stop hook ran")
	}
}

// TestOnPostStopLIFOOrder 验证 post-stop 钩子按注册逆序执行（与 defer /
// Wire cleanup 语义一致）。
func TestOnPostStopLIFOOrder(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	var mu sync.Mutex
	var order []string
	app.OnPostStop(func() { mu.Lock(); order = append(order, "a"); mu.Unlock() })
	app.OnPostStop(func() { mu.Lock(); order = append(order, "b"); mu.Unlock() })
	app.OnPostStop(func() { mu.Lock(); order = append(order, "c"); mu.Unlock() })

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
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

	want := []string{"c", "b", "a"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestOnPostStopRunsOnFailurePaths 验证失败退出路径同样执行 post-stop
// 钩子：服务 Init 失败与 OnPreStart 失败都经 Run 顶部的 defer 收尾。
func TestOnPostStopRunsOnFailurePaths(t *testing.T) {
	t.Run("init failure", func(t *testing.T) {
		app, err := newLynx(NewOptions())
		if err != nil {
			t.Fatalf("newLynx() error = %v", err)
		}
		ran := false
		app.Register(&failInitService{name: "bad", err: errors.New("init boom")})
		app.OnPostStop(func() { ran = true })
		if err := app.Run(); err == nil {
			t.Fatal("expected init error")
		}
		if !ran {
			t.Fatal("post-stop hook did not run after service init failure")
		}
	})

	t.Run("pre-start failure", func(t *testing.T) {
		app, err := newLynx(NewOptions())
		if err != nil {
			t.Fatalf("newLynx() error = %v", err)
		}
		ran := false
		app.Register(&blockingService{name: "c"})
		app.OnPreStart(func(ctx context.Context) error { return errors.New("hook boom") })
		app.OnPostStop(func() { ran = true })
		if err := app.Run(); err == nil {
			t.Fatal("expected pre-start hook error")
		}
		if !ran {
			t.Fatal("post-stop hook did not run after pre-start hook failure")
		}
	})
}

// TestOnPostStopTimeoutSkipsRemaining 验证 CleanupTimeout 预算：挂起的
// 钩子不阻塞进程退出，剩余钩子被跳过。
func TestOnPostStopTimeoutSkipsRemaining(t *testing.T) {
	app, err := newLynx(NewOptions(WithCleanupTimeout(100 * time.Millisecond)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	var skipped atomic.Bool
	// LIFO：blocker（最后注册）先执行并挂起；skipped 永远不应执行。
	app.OnPostStop(func() { skipped.Store(true) })
	app.OnPostStop(func() { time.Sleep(10 * time.Second) }) // 故意挂起

	start := time.Now()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	time.Sleep(50 * time.Millisecond)
	app.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil (cleanup timeout is log-only)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return; hanging post-stop hook was not bounded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdown took %v, want bounded by cleanup timeout", elapsed)
	}
	if skipped.Load() {
		t.Error("hook after the hanging one was executed despite cleanup timeout")
	}
}

// TestOnPostStopCloseBeforeRunRunsOnce 验证 Close 兜底与恰好一次语义：
// Run 从未调用时 Close 执行钩子；Run 已执行过后 Close 不重复执行。
func TestOnPostStopCloseBeforeRunRunsOnce(t *testing.T) {
	t.Run("close without run", func(t *testing.T) {
		app, err := newLynx(NewOptions())
		if err != nil {
			t.Fatalf("newLynx() error = %v", err)
		}
		var calls atomic.Int32
		app.OnPostStop(func() { calls.Add(1) })
		app.Close()
		if got := calls.Load(); got != 1 {
			t.Fatalf("post-stop calls after Close = %d, want 1", got)
		}
	})

	t.Run("close after run does not re-run", func(t *testing.T) {
		app, err := newLynx(NewOptions())
		if err != nil {
			t.Fatalf("newLynx() error = %v", err)
		}
		var calls atomic.Int32
		app.Register(&blockingService{name: "c"})
		app.OnPostStop(func() { calls.Add(1) })

		runErr := make(chan error, 1)
		go func() { runErr <- app.Run() }()
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
		if got := calls.Load(); got != 1 {
			t.Fatalf("post-stop calls = %d, want exactly 1", got)
		}
		app.Close() // 关停中断路径已调用过 Close，不得再次执行钩子
		if got := calls.Load(); got != 1 {
			t.Fatalf("post-stop calls after second Close = %d, want 1", got)
		}
	})
}

// TestRunnerSetupFailureRunsPostStop 验证 Runner.RunE 在 setup 失败时经
// Close 兜底执行 post-stop 钩子（setup 中已注册的资源被释放）。
func TestRunnerSetupFailureRunsPostStop(t *testing.T) {
	var calls atomic.Int32
	runner := NewRunner(func(app App) error {
		app.OnPostStop(func() { calls.Add(1) })
		return errors.New("setup boom")
	})
	err := runner.RunE()
	if err == nil || !strings.Contains(err.Error(), "setup boom") {
		t.Fatalf("RunE() error = %v, want setup boom", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("post-stop calls after setup failure = %d, want 1", got)
	}
}

// 确保 recordingBus 满足 eventbus.Bus（编译期约束）。
var _ eventbus.Bus = (*recordingBus)(nil)
