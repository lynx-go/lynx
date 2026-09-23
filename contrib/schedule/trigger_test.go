package schedule

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/cluster"
)

// TestTriggerFiresImmediately：Trigger 立即执行任务，无需等待 cron
// 时序（spec 取每年一次，确保计数只可能来自 Trigger）。
func TestTriggerFiresImmediately(t *testing.T) {
	var count atomic.Int32
	s, err := NewScheduler([]Task{
		newCountingTask("manual", "0 0 9 1 1 *", &count),
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	if err := s.Trigger("manual"); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}
	if got := count.Load(); got != 1 {
		t.Errorf("task ran %d times after Trigger, want 1", got)
	}
}

func TestTriggerUnknownTask(t *testing.T) {
	s, err := NewScheduler([]Task{
		newCountingTask("known", "0 0 9 1 1 *", &atomic.Int32{}),
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	err = s.Trigger("missing")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("Trigger(unknown) error = %v, want ErrTaskNotFound", err)
	}
}

// TestTriggerExclusiveRespectsMutex：固定时钟（WithNow）下连续两次
// Trigger 落在同一互斥格子，第二次被 TryOnce 略过——互斥行为可确定性
// 断言，无需真实等待。
func TestTriggerExclusiveRespectsMutex(t *testing.T) {
	coord := cluster.NewMemory(cluster.WithNamespace("trigger-test"))
	var count atomic.Int32
	fixed := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	s, err := NewScheduler(
		[]Task{Exclusive(newCountingTask("excl", "@every 5m", &count))},
		WithCoordinator(coord),
		WithNow(func() time.Time { return fixed }),
	)
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := s.Trigger("excl"); err != nil {
			t.Fatalf("Trigger() #%d error = %v", i, err)
		}
	}
	if got := count.Load(); got != 1 {
		t.Errorf("exclusive task ran %d times in one slot, want 1 (second fire skipped)", got)
	}
	// 时钟步进到下一格子（5m 间隔）后再次 Trigger 应执行。
	fixed = fixed.Add(6 * time.Minute)
	if err := s.Trigger("excl"); err != nil {
		t.Fatalf("Trigger() after advance error = %v", err)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("exclusive task ran %d times after slot advance, want 2", got)
	}
}

// TestTriggerReportsTaskError：任务返回错误经 OnTaskError 上报，
// Trigger 不把任务错误当自身错误返回（与 cron fire 行为一致）。
func TestTriggerReportsTaskError(t *testing.T) {
	taskErr := errors.New("boom")
	var gotErr error
	var gotName string
	s, err := NewScheduler([]Task{
		&testTask{name: "failing", cron: "0 0 9 1 1 *", handler: func(context.Context) error {
			return taskErr
		}},
	}, WithErrorHandler(func(_ context.Context, task Task, err error) {
		gotErr, gotName = err, task.Name()
	}))
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	if err := s.Trigger("failing"); err != nil {
		t.Fatalf("Trigger() error = %v, want nil (task error goes to OnTaskError)", err)
	}
	if !errors.Is(gotErr, taskErr) || gotName != "failing" {
		t.Errorf("OnTaskError got (%v, %q), want (boom, failing)", gotErr, gotName)
	}
}
