package lynx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestResolveProbeTiers 钉住三级解析优先级：Ready > Checker > 无信号；
// 同时实现 Ready 与 Checker 时 Ready 胜出。
func TestResolveProbeTiers(t *testing.T) {
	readyOnly := newReadyService("ready", 0, true, nil)
	p := resolveProbe(readyOnly)
	if p.ready == nil || p.checker != nil || p.empty() {
		t.Fatalf("Ready-only probe = %+v, want ready tier", p)
	}

	both := newReadyService("both", 0, true, &sequenceChecker{})
	p = resolveProbe(both)
	if p.ready == nil || p.checker != nil {
		t.Fatalf("Ready+Checker probe = %+v, want Ready tier to win", p)
	}

	checkerOnly := &depService{name: "checker", checker: &sequenceChecker{}}
	p = resolveProbe(checkerOnly)
	if p.checker == nil || p.ready != nil {
		t.Fatalf("Checker-only probe = %+v, want checker tier", p)
	}

	noSignal := &seqProbe{name: "none", log: &orderLog{}}
	if p = resolveProbe(noSignal); !p.empty() {
		t.Fatalf("no-signal probe = %+v, want empty", p)
	}
}

// TestProbeOnce 钉住单次探测（Command 每轮）语义：三级来源 + 有界 + ctx
// 取消优先于探测结果。
func TestProbeOnce(t *testing.T) {
	t.Run("ready closed", func(t *testing.T) {
		s := newReadyService("r", 0, false, nil)
		select {
		case <-s.Ready():
		case <-time.After(time.Second):
			t.Fatal("ready channel should close")
		}
		if err := resolveProbe(s).once(context.Background(), time.Second); err != nil {
			t.Fatalf("once = %v, want nil", err)
		}
	})

	t.Run("ready never closes is bounded", func(t *testing.T) {
		s := newReadyService("r", 0, true, nil)
		start := time.Now()
		err := resolveProbe(s).once(context.Background(), 30*time.Millisecond)
		if !errors.Is(err, errReadyNotClosed) {
			t.Fatalf("once = %v, want errReadyNotClosed", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("once took %v, want bounded by perCall", elapsed)
		}
	})

	t.Run("checker healthy", func(t *testing.T) {
		s := &depService{name: "c", checker: &sequenceChecker{}}
		if err := resolveProbe(s).once(context.Background(), time.Second); err != nil {
			t.Fatalf("once = %v, want nil", err)
		}
	})

	t.Run("checker unhealthy", func(t *testing.T) {
		s := &depService{name: "c", checker: &sequenceChecker{failures: 1}}
		if err := resolveProbe(s).once(context.Background(), time.Second); err == nil {
			t.Fatal("once = nil, want unhealthy error")
		}
	})

	t.Run("no signal is ready", func(t *testing.T) {
		s := &seqProbe{name: "n", log: &orderLog{}}
		if err := resolveProbe(s).once(context.Background(), time.Second); err != nil {
			t.Fatalf("once = %v, want nil (invoke means ready)", err)
		}
	})

	t.Run("hung checker bounded by perCall", func(t *testing.T) {
		c := &hungChecker{release: make(chan struct{})}
		defer close(c.release)
		start := time.Now()
		err := checkerProbe(c).once(context.Background(), 30*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "health check timed out after 30ms") {
			t.Fatalf("once = %v, want per-call timeout cause", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("once took %v, want bounded", elapsed)
		}
	})

	t.Run("cancel wins over healthy checker", func(t *testing.T) {
		s := &depService{name: "c", checker: &sequenceChecker{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := resolveProbe(s).once(ctx, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("once = %v, want context.Canceled (cancel priority)", err)
		}
	})

	t.Run("cancel aborts ready wait", func(t *testing.T) {
		s := newReadyService("r", 0, true, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := resolveProbe(s).once(ctx, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("once = %v, want context.Canceled", err)
		}
	})
}

// TestProbeWait 钉住预算循环（OrderedServices / 总线）语义：三级来源、
// startErr 交错（含 peek 放回）、预算耗尽经 errTimeout 包装、取消优先。
func TestProbeWait(t *testing.T) {
	t.Run("ready closes within budget", func(t *testing.T) {
		s := newReadyService("r", 30*time.Millisecond, false, nil)
		if err := resolveProbe(s).wait(context.Background(), 2*time.Second, nil, nil); err != nil {
			t.Fatalf("wait = %v, want nil", err)
		}
	})

	t.Run("checker becomes healthy", func(t *testing.T) {
		c := &sequenceChecker{failures: 1}
		s := &depService{name: "c", checker: c}
		if err := resolveProbe(s).wait(context.Background(), 2*time.Second, nil, nil); err != nil {
			t.Fatalf("wait = %v, want nil", err)
		}
		if got := c.Calls(); got < 2 {
			t.Errorf("health checked %d times, want >= 2 (poll until healthy)", got)
		}
	})

	t.Run("no signal with nil startErr", func(t *testing.T) {
		s := &seqProbe{name: "n", log: &orderLog{}}
		if err := resolveProbe(s).wait(context.Background(), time.Second, nil, nil); err != nil {
			t.Fatalf("wait = %v, want nil (invoke means ready)", err)
		}
	})

	t.Run("startErr surfaces and is peeked back", func(t *testing.T) {
		boom := errors.New("start boom")
		startErr := make(chan error, 1)
		startErr <- boom
		s := &seqProbe{name: "n", log: &orderLog{}}
		err := resolveProbe(s).wait(context.Background(), time.Second, startErr, nil)
		if !errors.Is(err, boom) {
			t.Fatalf("wait = %v, want start error", err)
		}
		select {
		case got := <-startErr:
			if !errors.Is(got, boom) {
				t.Fatalf("startErr after wait = %v, want peeked back", got)
			}
		default:
			t.Fatal("startErr must be put back for the final wait")
		}
	})

	t.Run("budget exhaustion wraps via errTimeout", func(t *testing.T) {
		c := &hungChecker{release: make(chan struct{})}
		defer close(c.release)
		wrap := errors.New("wrapped budget")
		err := checkerProbe(c).wait(context.Background(), 30*time.Millisecond, nil,
			func(last error) error { return wrap })
		if !errors.Is(err, wrap) {
			t.Fatalf("wait = %v, want errTimeout wrapper", err)
		}
	})

	t.Run("nil errTimeout returns raw cause", func(t *testing.T) {
		c := &hungChecker{release: make(chan struct{})}
		defer close(c.release)
		err := checkerProbe(c).wait(context.Background(), 30*time.Millisecond, nil, nil)
		if err == nil {
			t.Fatal("wait = nil, want budget exhaustion error")
		}
	})

	t.Run("cancel priority over budget", func(t *testing.T) {
		c := &hungChecker{release: make(chan struct{})}
		defer close(c.release)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)
		start := time.Now()
		err := checkerProbe(c).wait(ctx, time.Hour, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("wait took %v, want prompt abort on cancel", elapsed)
		}
	})
}

// TestAwaitHealthyNonPositiveBudget 锁定「预算 <= 0 时至少检查一次」的
// 既有语义在有界化后仍成立：即使预算已耗尽，也先执行有界检查
// （defaultProbeTimeout 兜底），而不是直接判超时。Windows 粗粒度时钟下
// 的 tick 与 deadline 判定可能不触发，允许在预算边界上多轮询一次，
// 因此断言「至少一次」而非「恰好一次」。
func TestAwaitHealthyNonPositiveBudget(t *testing.T) {
	healthy := &sequenceChecker{}
	if err := awaitHealthy(context.Background(), healthy, 0, readinessPollInterval, nil,
		func(last error) error { return last }); err != nil {
		t.Fatalf("awaitHealthy(healthy) = %v, want nil from a bounded check", err)
	}
	if got := healthy.Calls(); got < 1 {
		t.Errorf("CheckHealth called %d times, want at least 1", got)
	}

	failing := &sequenceChecker{failures: 100}
	if err := awaitHealthy(context.Background(), failing, 0, readinessPollInterval, nil,
		func(last error) error { return last }); err == nil {
		t.Fatal("awaitHealthy(failing) = nil, want timeout error from failed checks")
	}
	if got := failing.Calls(); got < 1 {
		t.Errorf("CheckHealth called %d times, want at least 1", got)
	}
}
