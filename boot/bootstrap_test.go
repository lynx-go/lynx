package boot_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/spf13/viper"
)

// fakeLynx is a minimal lynx.App implementation that records registration calls.
type fakeLynx struct {
	onStarts   []lynx.HookFunc
	onStops    []lynx.HookFunc
	components []lynx.Component
	factories  []lynx.ComponentFactory
}

func (f *fakeLynx) OnStart(fns ...lynx.HookFunc) { f.onStarts = append(f.onStarts, fns...) }
func (f *fakeLynx) OnStop(fns ...lynx.HookFunc)  { f.onStops = append(f.onStops, fns...) }
func (f *fakeLynx) Register(cs ...lynx.Component) {
	f.components = append(f.components, cs...)
}
func (f *fakeLynx) RegisterFactories(fs ...lynx.ComponentFactory) {
	f.factories = append(f.factories, fs...)
}

func (f *fakeLynx) Close()                             {}
func (f *fakeLynx) Config() lynx.Config                { return lynx.NewViperConfig(viper.New()) }
func (f *fakeLynx) Context() context.Context           { return context.Background() }
func (f *fakeLynx) Command(cmd lynx.CommandFunc) error { return nil }
func (f *fakeLynx) Run() error                         { return nil }
func (f *fakeLynx) SetLogger(logger *slog.Logger)      {}
func (f *fakeLynx) Logger(kwargs ...any) *slog.Logger  { return slog.Default() }
func (f *fakeLynx) HealthCheckers() []lynx.Checker     { return nil }

var _ lynx.App = (*fakeLynx)(nil)

func TestNew(t *testing.T) {
	onStarts := boot.OnStartHooks{func(ctx context.Context) error { return nil }}
	onStops := boot.OnStopHooks{func(ctx context.Context) error { return nil }}

	b := boot.New(onStarts, onStops, nil, nil)
	if b == nil {
		t.Fatal("New() returned nil")
	}
	if len(b.StartHooks) != 1 {
		t.Errorf("len(StartHooks) = %d, want 1", len(b.StartHooks))
	}
	if len(b.StopHooks) != 1 {
		t.Errorf("len(StopHooks) = %d, want 1", len(b.StopHooks))
	}
}

func TestBindRegistersAll(t *testing.T) {
	var startRan, stopRan bool
	onStarts := boot.OnStartHooks{func(ctx context.Context) error { startRan = true; return nil }}
	onStops := boot.OnStopHooks{func(ctx context.Context) error { stopRan = true; return nil }}
	b := boot.New(onStarts, onStops, nil, nil)
	app := &fakeLynx{}

	b.Bind(app)

	if len(app.onStarts) != 1 || len(app.onStops) != 1 {
		t.Fatalf("Bind() registered %d starts / %d stops, want 1/1",
			len(app.onStarts), len(app.onStops))
	}
	_ = app.onStarts[0](context.Background())
	_ = app.onStops[0](context.Background())
	if !startRan || !stopRan {
		t.Error("registered hooks should run")
	}
}

// TestBindNilSlices is a regression test: Bind must not panic when all
// providers are nil (modules with nothing to register).
func TestBindNilSlices(t *testing.T) {
	b := boot.New(nil, nil, nil, nil)
	app := &fakeLynx{}

	b.Bind(app)
}
