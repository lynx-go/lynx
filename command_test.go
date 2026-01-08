package lynx

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/spf13/viper"
	"gocloud.dev/server/health"
)

func TestNewCommand(t *testing.T) {
	cmd := NewCommand(func(ctx context.Context) error {
		return nil
	})

	if cmd.Name() != "command" {
		t.Errorf("expected name 'command', got %s", cmd.Name())
	}

	if cmd == nil {
		t.Error("expected non-nil command")
	}
}

func TestCommand_Init(t *testing.T) {
	mockApp := &mockLynx{}
	cmd := NewCommand(func(ctx context.Context) error {
		return nil
	})

	err := cmd.Init(mockApp)

	if err != nil {
		t.Errorf("unexpected error during Init: %v", err)
	}

	if cmd.(*command).lynx == nil {
		t.Error("expected app to be set in command")
	}
}

func TestCommand_Start(t *testing.T) {
	t.Run("successful execution with healthy components", func(t *testing.T) {
		var executed bool
		mockApp := &mockLynx{
			healthCheckers: []health.Checker{
				&HealthChecker{healthy: true},
			},
		}
		cmd := NewCommand(func(ctx context.Context) error {
			executed = true
			return nil
		})

		_ = cmd.Init(mockApp)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := cmd.Start(ctx)

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if !executed {
			t.Error("expected command to be executed")
		}
	})

	t.Run("waits for unhealthy components", func(t *testing.T) {
		var executed bool
		checker := &HealthChecker{healthy: false}
		mockApp := &mockLynx{
			healthCheckers: []health.Checker{checker},
		}
		cmd := NewCommand(func(ctx context.Context) error {
			executed = true
			return nil
		})

		_ = cmd.Init(mockApp)

		go func() {
			time.Sleep(100 * time.Millisecond)
			checker.SetHealthy(true)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		err := cmd.Start(ctx)

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if !executed {
			t.Error("expected command to be executed after health check passes")
		}
	})

	t.Run("command returns error", func(t *testing.T) {
		expectedErr := errors.New("command error")
		mockApp := &mockLynx{
			healthCheckers: []health.Checker{
				&HealthChecker{healthy: true},
			},
		}
		cmd := NewCommand(func(ctx context.Context) error {
			return expectedErr
		})

		_ = cmd.Init(mockApp)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := cmd.Start(ctx)

		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})

	t.Run("context cancelled", func(t *testing.T) {
		mockApp := &mockLynx{
			healthCheckers: []health.Checker{
				&HealthChecker{healthy: false},
			},
		}
		cmd := NewCommand(func(ctx context.Context) error {
			return nil
		})

		_ = cmd.Init(mockApp)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := cmd.Start(ctx)

		if err == nil {
			t.Error("expected error when context is cancelled")
		}
	})
}

func TestCommand_Stop(t *testing.T) {
	t.Run("stop closes app", func(t *testing.T) {
		mockApp := &mockLynx{}
		cmd := NewCommand(func(ctx context.Context) error {
			return nil
		})

		_ = cmd.Init(mockApp)
		ctx := context.Background()

		cmd.Stop(ctx)

		if !mockApp.closed {
			t.Error("expected app to be closed on stop")
		}
	})
}

type mockLynx struct {
	closed         bool
	healthCheckers []health.Checker
}

func (m *mockLynx) Close() {
	m.closed = true
}

func (m *mockLynx) Config() *viper.Viper {
	return viper.New()
}

func (m *mockLynx) Context() context.Context {
	return context.Background()
}

func (m *mockLynx) CLI(cmd CommandFunc) error {
	return nil
}

func (m *mockLynx) Hooks(hooks ...HookOption) error {
	return nil
}

func (m *mockLynx) HealthCheckFunc() HealthCheckFunc {
	return func() []health.Checker {
		return m.healthCheckers
	}
}

func (m *mockLynx) Run() error {
	return nil
}

func (m *mockLynx) SetLogger(logger *slog.Logger) {
}

func (m *mockLynx) Logger(kwargs ...any) *slog.Logger {
	return slog.Default()
}
