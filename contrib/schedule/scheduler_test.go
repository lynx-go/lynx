package schedule

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func TestTask(t *testing.T) {
	t.Run("task implementation", func(t *testing.T) {
		task := &mockTask{
			name: "test-task",
			cron: "*/5 * * * * *",
		}

		if task.Name() != "test-task" {
			t.Errorf("expected name = test-task, got %s", task.Name())
		}

		if task.Cron() != "*/5 * * * * *" {
			t.Errorf("expected cron = */5 * * * * *, got %s", task.Cron())
		}

		handler := task.HandlerFunc()
		if handler == nil {
			t.Error("expected non-nil handler function")
		}

		ctx := context.Background()
		if err := handler(ctx); err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if !task.executed {
			t.Error("expected task to be executed")
		}
	})

	t.Run("task returns error", func(t *testing.T) {
		expectedErr := &testError{message: "task error"}
		task := &mockTask{
			name:  "error-task",
			cron:  "0 * * * * *",
			error: expectedErr,
		}

		handler := task.HandlerFunc()
		err := handler(context.Background())

		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})
}

func TestNewScheduler(t *testing.T) {
	t.Run("simple scheduler", func(t *testing.T) {
		task := &mockTask{name: "task1", cron: "0 * * * * *"}
		scheduler, err := NewScheduler([]Task{task})

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if scheduler.Name() != "cron-scheduler" {
			t.Errorf("expected name = cron-scheduler, got %s", scheduler.Name())
		}

		if scheduler.tasks == nil {
			t.Error("expected tasks to be initialized")
		}

		if len(scheduler.tasks) != 1 {
			t.Errorf("expected 1 task, got %d", len(scheduler.tasks))
		}
	})

	t.Run("multiple tasks", func(t *testing.T) {
		tasks := []Task{
			&mockTask{name: "task1", cron: "0 * * * * *"},
			&mockTask{name: "task2", cron: "*/5 * * * * *"},
			&mockTask{name: "task3", cron: "0 0 * * * *"},
		}

		scheduler, err := NewScheduler(tasks)

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if len(scheduler.tasks) != 3 {
			t.Errorf("expected 3 tasks, got %d", len(scheduler.tasks))
		}
	})

	t.Run("invalid cron expression", func(t *testing.T) {
		task := &mockTask{name: "invalid-task", cron: "invalid-cron"}
		_, err := NewScheduler([]Task{task})

		if err == nil {
			t.Error("expected error for invalid cron expression")
		}
	})
}

func TestScheduler_Lifecycle(t *testing.T) {
	t.Run("init and check health", func(t *testing.T) {
		task := &mockTask{name: "health-task", cron: "0 * * * * *"}
		scheduler, _ := NewScheduler([]Task{task})

		err := scheduler.CheckHealth()
		if err != nil {
			t.Errorf("expected healthy, got error: %v", err)
		}
	})

	t.Run("start and stop", func(t *testing.T) {
		task := &mockTask{name: "lifecycle-task", cron: "*/1 * * * * *"}
		scheduler, _ := NewScheduler([]Task{task})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- scheduler.Start(ctx)
		}()

		time.Sleep(500 * time.Millisecond)
		scheduler.Stop(ctx)

		select {
		case err := <-errCh:
			if err != nil && err != context.DeadlineExceeded {
				t.Errorf("unexpected error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("timeout waiting for scheduler to stop")
		}
	})
}

func TestSchedulerOptions(t *testing.T) {
	t.Run("WithLogger", func(t *testing.T) {
		logger := slog.Default()
		task := &mockTask{name: "task1", cron: "0 * * * * *"}
		scheduler, err := NewScheduler([]Task{task}, WithLogger(logger))

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if scheduler.options.Logger == nil {
			t.Error("expected logger to be set")
		}
	})

	t.Run("WithDebugEnabled", func(t *testing.T) {
		task := &mockTask{name: "task1", cron: "0 * * * * *"}
		scheduler, err := NewScheduler([]Task{task}, WithDebugEnabled())

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if !scheduler.options.DebugEnabled {
			t.Error("expected DebugEnabled to be true")
		}
	})

	t.Run("WithCron", func(t *testing.T) {
		customCron := cron.New(cron.WithSeconds())
		task := &mockTask{name: "task1", cron: "*/1 * * * * *"}
		scheduler, err := NewScheduler([]Task{task}, WithCron(customCron))

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if scheduler.options.Cron != customCron {
			t.Error("expected custom cron to be set")
		}
	})
}

func TestNewSlogLogger(t *testing.T) {
	t.Run("create logger without debug", func(t *testing.T) {
		slogger := slog.Default()
		logger := NewSlogLogger(slogger, false)

		if logger == nil {
			t.Error("expected non-nil logger")
		}

		logger.Info("test message", "key", "value")
	})

	t.Run("create logger with debug enabled", func(t *testing.T) {
		slogger := slog.Default()
		logger := NewSlogLogger(slogger, true)

		if logger == nil {
			t.Error("expected non-nil logger")
		}

		logger.Info("test message", "key", "value")
	})

	t.Run("error logging", func(t *testing.T) {
		slogger := slog.Default()
		logger := NewSlogLogger(slogger, false)
		testErr := &testError{message: "test error"}

		logger.Error(testErr, "error message", "key", "value")
	})
}

type mockTask struct {
	name     string
	cron     string
	error    error
	executed bool
}

func (m *mockTask) Name() string {
	return m.name
}

func (m *mockTask) Cron() string {
	return m.cron
}

func (m *mockTask) HandlerFunc() HandlerFunc {
	return func(ctx context.Context) error {
		m.executed = true
		return m.error
	}
}

type testError struct {
	message string
}

func (e *testError) Error() string {
	return e.message
}
