package lynx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// teardownProbe 记录 Stop 的调用顺序与收到 ctx 时的存活状态，并可返回固定
// 错误：用于关停快路径契约（逆序停止、ctx 未被提前取消、错误聚合）的断言。
type teardownProbe struct {
	name    string
	rec     *eventRecorder
	stopErr error
	ctxLive atomic.Bool
}

func (s *teardownProbe) Name() string                    { return s.name }
func (s *teardownProbe) Init(ctx AppContext) error       { return nil }
func (s *teardownProbe) Start(ctx context.Context) error { <-ctx.Done(); return nil }

func (s *teardownProbe) Stop(ctx context.Context) error {
	if ctx.Err() == nil {
		s.ctxLive.Store(true)
	}
	if s.rec != nil {
		s.rec.record("stop:" + s.name)
	}
	return s.stopErr
}

// closeRecordingBus 记录 Stop 是否被调用并可返回固定错误，用于 Close
// 无返回值路径的「总线已停 + 错误进入聚合」断言。
type closeRecordingBus struct {
	readyGateBus
	mu      sync.Mutex
	stopped bool
	stopErr error
}

func (b *closeRecordingBus) Name() string { return "close-recording-bus" }
func (b *closeRecordingBus) Stop(ctx context.Context) error {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	return b.stopErr
}
func (b *closeRecordingBus) isStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

// countEvent 统计事件出现次数（幂等断言用：不得重复 Stop）。
func countEvent(events []string, want string) int {
	n := 0
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

// TestRunEarlyExitFastTeardown 钉住启动期早退的关停快路径契约：
// 触发错误与关停错误聚合上抛、服务逆序停止且各恰一次、Stop 收到存活 ctx、
// 应用 ctx 在返回前取消、排水与 OnPreStop 不执行、OnPostStop 仍执行一次。
func TestRunEarlyExitFastTeardown(t *testing.T) {
	triggerBoom := errors.New("trigger boom")
	cases := []struct {
		name         string
		opts         []Option
		setup        func(app App, s1, s2 Service)
		want         error  // errors.Is 断言；nil 时跳过
		wantContains string // 错误文案断言；空时跳过
		drainTrigger bool   // true 时不启用排水窗口（该用例的触发即「有钩子无预算」）
	}{
		{
			name: "init failure",
			setup: func(app App, s1, s2 Service) {
				app.Register(s1, s2, &failInitService{name: "bad", err: triggerBoom})
			},
			want: triggerBoom,
		},
		{
			name: "drain hooks without budget",
			setup: func(app App, s1, s2 Service) {
				app.Register(s1, s2)
				app.OnDrain(func(ctx context.Context) error { return nil })
			},
			want:         ErrDrainHooksRequireDrainTimeout,
			drainTrigger: true,
		},
		{
			name: "config watch without config file",
			opts: []Option{WithConfigWatch()},
			setup: func(app App, s1, s2 Service) {
				app.Register(s1, s2)
			},
			wantContains: "config file",
		},
		{
			name: "on-pre-start failure",
			setup: func(app App, s1, s2 Service) {
				app.Register(s1, s2)
				app.OnPreStart(func(ctx context.Context) error { return triggerBoom })
			},
			want: triggerBoom,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &eventRecorder{}
			s1 := &teardownProbe{name: "s1", rec: rec, stopErr: errors.New("stop boom")}
			s2 := &teardownProbe{name: "s2", rec: rec, stopErr: errors.New("stop boom")}
			opts := append([]Option{}, tc.opts...)
			if !tc.drainTrigger {
				// 非触发用例启用排水窗口并注册 OnDrain，验证快路径整段跳过。
				opts = append(opts, WithDrainTimeout(30*time.Millisecond))
			}
			app, err := newLynx(NewOptions(opts...))
			if err != nil {
				t.Fatalf("newLynx() error = %v", err)
			}
			tc.setup(app, s1, s2)
			app.OnDrain(func(ctx context.Context) error { rec.record("ondrain"); return nil })
			app.OnPreStop(func(ctx context.Context) error { rec.record("onprestop"); return nil })
			var posts atomic.Int32
			app.OnPostStop(func() { posts.Add(1) })

			runErr := app.Run()
			if runErr == nil {
				t.Fatal("Run() = nil, want trigger error")
			}
			if tc.want != nil && !errors.Is(runErr, tc.want) {
				t.Fatalf("Run() = %v, want wrapped %v", runErr, tc.want)
			}
			if tc.wantContains != "" && !strings.Contains(runErr.Error(), tc.wantContains) {
				t.Fatalf("Run() = %v, want contains %q", runErr, tc.wantContains)
			}
			if !strings.Contains(runErr.Error(), "stop boom") {
				t.Fatalf("Run() = %v, want joined service stop error", runErr)
			}

			events := rec.snapshot()
			assertBefore(t, events, "stop:s2", "stop:s1")
			for _, name := range []string{"stop:s1", "stop:s2"} {
				if got := countEvent(events, name); got != 1 {
					t.Errorf("%s recorded %d times, want exactly 1 (no double Stop)", name, got)
				}
			}
			if !s1.ctxLive.Load() || !s2.ctxLive.Load() {
				t.Error("Stop must receive a live app ctx on the fast path")
			}
			select {
			case <-app.Context().Done():
			default:
				t.Error("app ctx must be cancelled before Run returns")
			}
			if got := countEvent(events, "ondrain"); got != 0 {
				t.Errorf("OnDrain ran %d times on fast path, want 0", got)
			}
			if got := countEvent(events, "onprestop"); got != 0 {
				t.Errorf("OnPreStop ran %d times on fast path, want 0", got)
			}
			if posts.Load() != 1 {
				t.Errorf("OnPostStop ran %d times, want exactly 1", posts.Load())
			}
		})
	}
}

// TestCloseBeforeRunFastTeardown 钉住 Close 兜底快路径：逆序有界停止已
// Init 的服务且各恰一次、Stop 收到存活 ctx、总线停止且错误进入聚合
// （Close 无返回值）、应用 ctx 取消、OnPostStop 恰一次。
func TestCloseBeforeRunFastTeardown(t *testing.T) {
	rec := &eventRecorder{}
	busBoom := errors.New("bus stop boom")
	s1 := &teardownProbe{name: "s1", rec: rec, stopErr: errors.New("stop boom")}
	s2 := &teardownProbe{name: "s2", rec: rec}
	bus := &closeRecordingBus{stopErr: busBoom}
	app, err := newLynx(NewOptions(WithBus(bus)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.Register(s1, s2)
	var posts atomic.Int32
	app.OnPostStop(func() { posts.Add(1) })

	app.Close()

	events := rec.snapshot()
	assertBefore(t, events, "stop:s2", "stop:s1")
	if got := countEvent(events, "stop:s1"); got != 1 {
		t.Errorf("stop:s1 recorded %d times, want exactly 1", got)
	}
	if !s1.ctxLive.Load() || !s2.ctxLive.Load() {
		t.Error("Stop must receive a live app ctx on Close fast path")
	}
	if !bus.isStopped() {
		t.Error("bus must be stopped by Close")
	}
	select {
	case <-app.Context().Done():
	default:
		t.Error("app ctx must be cancelled before Close returns")
	}
	if posts.Load() != 1 {
		t.Errorf("OnPostStop ran %d times, want exactly 1", posts.Load())
	}
	l := app.(*lynx)
	if !errors.Is(&l.shutdownErrors, busBoom) {
		t.Errorf("bus stop error not aggregated: %v", l.shutdownErrors.Errors())
	}
	if !errors.Is(&l.shutdownErrors, s1.stopErr) {
		t.Errorf("service stop error not aggregated: %v", l.shutdownErrors.Errors())
	}
}

// TestCloseAfterInitFailureDoesNotRestop 钉住批次幂等：Init 失败时
// addServices 已停止先前服务，Close 兜底不得重复调用 Stop。
func TestCloseAfterInitFailureDoesNotRestop(t *testing.T) {
	rec := &eventRecorder{}
	s1 := &teardownProbe{name: "s1", rec: rec}
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	app.Register(s1, &failInitService{name: "bad", err: errors.New("init boom")})

	app.Close()

	if got := countEvent(rec.snapshot(), "stop:s1"); got != 1 {
		t.Errorf("stop:s1 recorded %d times, want exactly 1", got)
	}
}
