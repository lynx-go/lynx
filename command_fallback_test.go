package lynx_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/lynxtest"
)

// failOnceChecker fails the first CheckHealth then succeeds（等价于包内
// sequenceChecker：验证外部 AppContext 回退路径的健康检查聚合）。
type failOnceChecker struct {
	mu    sync.Mutex
	calls int
}

func (c *failOnceChecker) CheckHealth() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= 1 {
		return errors.New("not ready")
	}
	return nil
}

func (c *failOnceChecker) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCommandFallbackExternalAppContext：appctx 非 *lynx（外部 AppContext
// 实现，这里用 lynxtest.NewContext）时，依赖等待回退 HealthCheckers 聚合，
// 行为与既有 Checker 轮询一致。本用例同时验证
// lynxtest.ContextWithCheckers 的注入路径可在包外使用。
func TestCommandFallbackExternalAppContext(t *testing.T) {
	checker := &failOnceChecker{}
	var ran atomic.Int32
	cmd := lynx.NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, lynx.WithMaxTries(5), lynx.WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(lynxtest.NewContext(t, lynxtest.ContextWithCheckers(checker))); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil after retry", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	if got := checker.Calls(); got != 2 {
		t.Errorf("health checked %d times, want 2 (1 failure + 1 success)", got)
	}
}
