package boot_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/spf13/viper"
	"gocloud.dev/server/health"
)

// fakeLynx is a minimal lynx.App implementation that records Hooks calls.
type fakeLynx struct {
	hooksCalls int
	failOnCall int // 1-based Hooks call index that should fail; 0 means never fail
}

func (f *fakeLynx) Hooks(hooks ...lynx.HookOption) error {
	f.hooksCalls++
	if f.hooksCalls == f.failOnCall {
		return errors.New("hooks failed")
	}
	return nil
}

func (f *fakeLynx) Close()                            {}
func (f *fakeLynx) Config() lynx.Config              { return lynx.NewViperConfig(viper.New()) }
func (f *fakeLynx) Context() context.Context          { return context.Background() }
func (f *fakeLynx) CLI(cmd lynx.CommandFunc) error    { return nil }
func (f *fakeLynx) Run() error                        { return nil }
func (f *fakeLynx) SetLogger(logger *slog.Logger)     {}
func (f *fakeLynx) Logger(kwargs ...any) *slog.Logger { return slog.Default() }
func (f *fakeLynx) HealthCheckFunc() lynx.HealthCheckFunc {
	return func() []health.Checker { return nil }
}

var _ lynx.App = (*fakeLynx)(nil)

func TestNew(t *testing.T) {
	onStarts := lynx.OnStartHooks{func(ctx context.Context) error { return nil }}
	onStops := lynx.OnStopHooks{func(ctx context.Context) error { return nil }}
	setFunc := func() lynx.ComponentBuilderSet { return nil }

	b := boot.New(onStarts, onStops, nil, nil, setFunc)
	if b == nil { //nolint:staticcheck // SA5011 关联信息：t.Fatal 已保证 b 非 nil
		t.Fatal("New() returned nil")
	}
	if len(b.StartHooks) != 1 { //nolint:staticcheck // SA5011 误报：上面的 t.Fatal 已保证 b 非 nil
		t.Errorf("len(StartHooks) = %d, want 1", len(b.StartHooks))
	}
	if len(b.StopHooks) != 1 {
		t.Errorf("len(StopHooks) = %d, want 1", len(b.StopHooks))
	}
	if b.ComponentBuilderSetFunc == nil {
		t.Error("ComponentBuilderSetFunc should be set")
	}
}

// TestBindNilComponentBuilderSetFunc is a regression test for commit 31e1db2:
// Bind must not panic when ComponentBuilderSetFunc is nil.
func TestBindNilComponentBuilderSetFunc(t *testing.T) {
	b := boot.New(nil, nil, nil, nil, nil)
	app := &fakeLynx{}

	if err := b.Bind(app); err != nil {
		t.Fatalf("Bind() error = %v, want nil", err)
	}
	// OnStart, OnStop, Components, ComponentBuilders — no ComponentBuilderSetFunc call.
	if app.hooksCalls != 4 {
		t.Errorf("Hooks() called %d times, want 4", app.hooksCalls)
	}
}

func TestBindWithComponentBuilderSetFunc(t *testing.T) {
	called := 0
	setFunc := func() lynx.ComponentBuilderSet {
		called++
		return nil
	}
	b := boot.New(nil, nil, nil, nil, setFunc)
	app := &fakeLynx{}

	if err := b.Bind(app); err != nil {
		t.Fatalf("Bind() error = %v, want nil", err)
	}
	if called != 1 {
		t.Errorf("ComponentBuilderSetFunc called %d times, want 1", called)
	}
	if app.hooksCalls != 5 {
		t.Errorf("Hooks() called %d times, want 5", app.hooksCalls)
	}
}

func TestBindHooksErrorPropagation(t *testing.T) {
	setFunc := func() lynx.ComponentBuilderSet { return nil }

	tests := []struct {
		name       string
		failOnCall int
		wantErr    bool
	}{
		{name: "on-start hooks fail", failOnCall: 1, wantErr: true},
		{name: "on-stop hooks fail", failOnCall: 2, wantErr: true},
		{name: "components hooks fail", failOnCall: 3, wantErr: true},
		{name: "component builders hooks fail", failOnCall: 4, wantErr: true},
		{name: "builder set hooks fail", failOnCall: 5, wantErr: true},
		{name: "no failure", failOnCall: 0, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := boot.New(nil, nil, nil, nil, setFunc)
			app := &fakeLynx{failOnCall: tt.failOnCall}
			err := b.Bind(app)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Bind() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
