package lynx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// assertBefore 断言 both 事件都已记录且 before 先于 after 出现。
func assertBefore(t *testing.T, events []string, before, after string) {
	t.Helper()
	i, j := indexOf(events, before), indexOf(events, after)
	if i < 0 || j < 0 {
		t.Fatalf("events = %v, want both %q and %q", events, before, after)
	}
	if i > j {
		t.Fatalf("events = %v, want %q before %q", events, before, after)
	}
}

// TestRunStartFailureOnPreStopBeforeServiceStop 回归候选 1：服务 Start 失败
// 触发的关停同样先跑 OnPreStop，再停止兄弟服务（此前顺序取决于 actor
// 注册顺序，OnPreStop 被挤到服务 Stop 之后）。
func TestRunStartFailureOnPreStopBeforeServiceStop(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	app.Register(&blockingService{name: "sib", record: rec.record})
	app.Register(&failStartService{name: "bad", err: errors.New("start boom")})
	app.OnPreStop(func(ctx context.Context) error {
		rec.record("onprestop")
		return nil
	})

	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "start boom") {
		t.Fatalf("Run() error = %v, want start boom", err)
	}
	assertBefore(t, rec.snapshot(), "onprestop", "stop:sib")
}

// TestRunPostStartHookErrorOnPreStopBeforeServiceStop 回归候选 1：OnPostStart
// 钩子失败触发的关停同样先跑 OnPreStop，再停止服务。
func TestRunPostStartHookErrorOnPreStopBeforeServiceStop(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	app.Register(&blockingService{name: "sib", record: rec.record})
	app.OnPostStart(func(ctx context.Context) error { return errors.New("poststart boom") })
	app.OnPreStop(func(ctx context.Context) error {
		rec.record("onprestop")
		return nil
	})

	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "poststart boom") {
		t.Fatalf("Run() error = %v, want poststart boom", err)
	}
	assertBefore(t, rec.snapshot(), "onprestop", "stop:sib")
}

// TestRunCommandCompletionOnPreStopBeforeServiceStop 回归候选 1：Command
// 正常完成（nil 返回）触发的关停同样先跑 OnPreStop，再停止其余服务。
func TestRunCommandCompletionOnPreStopBeforeServiceStop(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	app.Register(&blockingService{name: "sib", record: rec.record})
	app.OnPreStop(func(ctx context.Context) error {
		rec.record("onprestop")
		return nil
	})
	if err := app.Command(func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Command() error = %v", err)
	}

	if err := app.Run(); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	assertBefore(t, rec.snapshot(), "onprestop", "stop:sib")
}

// errStopBus 的 Stop 返回错误：锁定总线停止错误进入 Run 返回值
// （此前写在 Run 返回值计算之后的 defer 里，被静默丢弃）。
type errStopBus struct{ readyGateBus }

func (b *errStopBus) Name() string                   { return "err-stop-bus" }
func (b *errStopBus) Stop(ctx context.Context) error { return errors.New("bus stop boom") }

func TestRunBusStopErrorSurfaced(t *testing.T) {
	app, err := newLynx(NewOptions(WithBus(&errStopBus{})))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	time.Sleep(50 * time.Millisecond)
	app.Close()
	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), "bus stop boom") {
			t.Fatalf("Run() error = %v, want bus stop error surfaced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// failStartBus 的 Start 立即返回错误、CheckHealth 永不健康：锁定构造期
// 以 Start 根因快速失败（此前退化成长达 BusReadyTimeout 的就绪超时）。
type failStartBus struct {
	readyGateBus
	err error
}

func (b *failStartBus) Name() string                    { return "fail-start-bus" }
func (b *failStartBus) Start(ctx context.Context) error { return b.err }

func TestNewLynxBusStartErrorFailsFast(t *testing.T) {
	bus := &failStartBus{readyGateBus: readyGateBus{never: true}, err: errors.New("bus start boom")}
	start := time.Now()
	_, err := newLynx(NewOptions(WithBus(bus), WithBusReadyTimeout(5*time.Second)))
	if err == nil || !strings.Contains(err.Error(), "bus start boom") {
		t.Fatalf("newLynx() error = %v, want bus start root cause", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("newLynx() took %v, want fail fast before BusReadyTimeout", elapsed)
	}
}

// TestRegisterAfterCloseRejected 回归候选 1：Close 之后的注册必须被拒绝
// （此前 Register 会 Init 并登记，Run 因 closed 早退，服务永不 Stop）。
func TestRegisterAfterCloseRejected(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.Close()

	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("%s did not panic after Close()", name)
			}
		}()
		fn()
	}
	assertPanics("Register", func() { app.Register(&blockingService{name: "late"}) })
	assertPanics("RegisterFactories", func() { app.RegisterFactories(&recordingFactory{instances: 1}) })
	if err := app.Command(func(ctx context.Context) error { return nil }); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Command() error = %v, want ErrAppClosed", err)
	}
}

// TestCommandOptionsReachable 锁定 App.Command 的选项面（此前签名不接收
// CommandOption，WithMaxTries/WithProbeTimeout 从 App 入口不可达）。
func TestCommandOptionsReachable(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	ran := false
	if err := app.Command(func(ctx context.Context) error {
		ran = true
		return nil
	}, WithMaxTries(3), WithProbeTimeout(time.Second), WithCommandName("my-cmd")); err != nil {
		t.Fatalf("Command(opts) error = %v", err)
	}
	if err := app.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !ran {
		t.Fatal("command did not run")
	}
}

// TestRunInitFailureStopsBus 回归候选 1：启动期早退（Init 失败）同样有界
// 停止提前 Start 的总线（此前 Run 已置位 running，Close 不再兜底总线，
// 总线泄漏到进程退出）。
func TestRunInitFailureStopsBus(t *testing.T) {
	bus := &recordingBus{}
	app, err := newLynx(NewOptions(WithBus(bus)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.Register(&failInitService{name: "bad", err: errors.New("init boom")})

	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "init boom") {
		t.Fatalf("Run() error = %v, want init boom", err)
	}
	if !bus.isStopped() {
		t.Fatal("bus was not stopped after init failure")
	}
}
