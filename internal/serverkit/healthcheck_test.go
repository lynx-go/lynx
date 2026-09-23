package serverkit

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
)

// funcChecker 把函数适配为 lynx.Checker。
type funcChecker func() error

func (f funcChecker) CheckHealth() error { return f() }

// TestRunHealthChecksConcurrent 并发语义：两个各睡 150ms 的 checker 在
// 200ms 上限内通过——顺序执行（300ms）必然超时。
func TestRunHealthChecksConcurrent(t *testing.T) {
	slow := func() lynx.Checker {
		return funcChecker(func() error {
			time.Sleep(150 * time.Millisecond)
			return nil
		})
	}
	checkers := func() []lynx.Checker { return []lynx.Checker{slow(), slow()} }

	start := time.Now()
	if err := RunHealthChecks(checkers, 200*time.Millisecond); err != nil {
		t.Fatalf("并发执行下应在时限内全部通过: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Errorf("elapsed = %v, checker 未并发执行（顺序 300ms 应已超时）", elapsed)
	}
}

// TestRunHealthChecksTimeout 超时语义与错误文案。
func TestRunHealthChecksTimeout(t *testing.T) {
	hung := funcChecker(func() error {
		time.Sleep(2 * time.Second)
		return nil
	})
	err := RunHealthChecks(func() []lynx.Checker { return []lynx.Checker{hung} }, 50*time.Millisecond)
	if err == nil {
		t.Fatal("hung checker 应按超时不健康返回")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want 超时错误", err)
	}
}

// TestRunHealthChecksFirstErrorWins 任一失败立即返回其错误。
func TestRunHealthChecksFirstErrorWins(t *testing.T) {
	want := errors.New("checker boom")
	err := RunHealthChecks(func() []lynx.Checker {
		return []lynx.Checker{
			funcChecker(func() error { return nil }),
			funcChecker(func() error { return want }),
		}
	}, time.Second)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// TestRunHealthChecksPanicRecovered panic 兜底：按不健康返回，不拖垮进程。
func TestRunHealthChecksPanicRecovered(t *testing.T) {
	err := RunHealthChecks(func() []lynx.Checker {
		return []lynx.Checker{funcChecker(func() error { panic("checker exploded") })}
	}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want panic-recovered error", err)
	}
}

// TestRunHealthChecksEmpty 无 checker 恒通过。
func TestRunHealthChecksEmpty(t *testing.T) {
	if err := RunHealthChecks(func() []lynx.Checker { return nil }, time.Second); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestRunHealthChecksNonPositiveTimeoutSequential：timeout<=0 的顺序逃生口
// （保持旧行为：不做并发与限时）。
func TestRunHealthChecksNonPositiveTimeoutSequential(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mk := func(name string) lynx.Checker {
		return funcChecker(func() error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		})
	}
	err := RunHealthChecks(func() []lynx.Checker { return []lynx.Checker{mk("a"), mk("b")} }, 0)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v, want sequential [a b]", order)
	}
}
