package boot_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// fakeLynx is a minimal lynx.App implementation that records registration calls.
type fakeLynx struct {
	onPreStarts  []lynx.HookFunc
	onDrains     []lynx.HookFunc
	onPreStops   []lynx.HookFunc
	onPostStarts []lynx.HookFunc
	onPostStops  []lynx.CleanupFunc
	services     []lynx.Service
	factories    []lynx.ServiceFactory
}

func (f *fakeLynx) OnPreStart(fns ...lynx.HookFunc) { f.onPreStarts = append(f.onPreStarts, fns...) }
func (f *fakeLynx) OnDrain(fns ...lynx.HookFunc)    { f.onDrains = append(f.onDrains, fns...) }
func (f *fakeLynx) OnPreStop(fns ...lynx.HookFunc)  { f.onPreStops = append(f.onPreStops, fns...) }
func (f *fakeLynx) OnPostStart(fns ...lynx.HookFunc) {
	f.onPostStarts = append(f.onPostStarts, fns...)
}
func (f *fakeLynx) OnPostStop(fns ...lynx.CleanupFunc) {
	f.onPostStops = append(f.onPostStops, fns...)
}
func (f *fakeLynx) Register(cs ...lynx.Service) {
	f.services = append(f.services, cs...)
}
func (f *fakeLynx) RegisterFactories(fs ...lynx.ServiceFactory) {
	f.factories = append(f.factories, fs...)
}

func (f *fakeLynx) Close()                                                         {}
func (f *fakeLynx) Config() lynx.Config                                            { return lynx.NewViperConfig(viper.New()) }
func (f *fakeLynx) Context() context.Context                                       { return context.Background() }
func (f *fakeLynx) Command(cmd lynx.CommandFunc, opts ...lynx.CommandOption) error { return nil }
func (f *fakeLynx) Run() error                                                     { return nil }
func (f *fakeLynx) SetLogger(logger *slog.Logger)                                  {}
func (f *fakeLynx) Logger(kwargs ...any) *slog.Logger                              { return slog.Default() }
func (f *fakeLynx) HealthCheckers() []lynx.Checker                                 { return nil }
func (f *fakeLynx) Bus() eventbus.Bus                                              { return eventbus.NewMemoryBus(eventbus.Options{}) }

var _ lynx.App = (*fakeLynx)(nil)

func TestNew(t *testing.T) {
	preStarts := boot.PreStartHooks{func(ctx context.Context) error { return nil }}
	drains := boot.DrainHooks{func(ctx context.Context) error { return nil }}
	preStops := boot.PreStopHooks{func(ctx context.Context) error { return nil }}
	postStops := boot.PostStopHooks{func() {}}

	b := boot.New(preStarts, drains, preStops, postStops, nil, nil)
	if b == nil {
		t.Fatal("New() returned nil")
	}
	if len(b.PreStartHooks) != 1 {
		t.Errorf("len(PreStartHooks) = %d, want 1", len(b.PreStartHooks))
	}
	if len(b.DrainHooks) != 1 {
		t.Errorf("len(DrainHooks) = %d, want 1", len(b.DrainHooks))
	}
	if len(b.PreStopHooks) != 1 {
		t.Errorf("len(PreStopHooks) = %d, want 1", len(b.PreStopHooks))
	}
	if len(b.PostStopHooks) != 1 {
		t.Errorf("len(PostStopHooks) = %d, want 1", len(b.PostStopHooks))
	}
}

func TestApplyRegistersAll(t *testing.T) {
	var preStartRan, preStopRan, postStopRan bool
	preStarts := boot.PreStartHooks{func(ctx context.Context) error { preStartRan = true; return nil }}
	preStops := boot.PreStopHooks{func(ctx context.Context) error { preStopRan = true; return nil }}
	postStops := boot.PostStopHooks{func() { postStopRan = true }}
	b := boot.New(preStarts, nil, preStops, postStops, nil, nil)
	app := &fakeLynx{}

	b.Apply(app)

	if len(app.onPreStarts) != 1 || len(app.onPreStops) != 1 || len(app.onPostStops) != 1 {
		t.Fatalf("Apply() registered %d pre-starts / %d pre-stops / %d post-stops, want 1/1/1",
			len(app.onPreStarts), len(app.onPreStops), len(app.onPostStops))
	}
	_ = app.onPreStarts[0](context.Background())
	_ = app.onPreStops[0](context.Background())
	app.onPostStops[0]()
	if !preStartRan || !preStopRan || !postStopRan {
		t.Error("registered hooks should run")
	}
}

// TestApplyNilSlices is a regression test: Apply must not panic when all
// providers are nil (modules with nothing to register).
func TestApplyNilSlices(t *testing.T) {
	b := boot.New(nil, nil, nil, nil, nil, nil)
	app := &fakeLynx{}

	b.Apply(app)
}

// TestApplyDrainHooks 验证排水钩子经 New 直接传入并注册（v1.10.0 起
// drains 是 New 的正式参数，不再需要 setter）。
func TestApplyDrainHooks(t *testing.T) {
	var drainRan bool
	drains := boot.DrainHooks{func(ctx context.Context) error { drainRan = true; return nil }}
	b := boot.New(nil, drains, nil, nil, nil, nil)
	if len(b.DrainHooks) != 1 {
		t.Fatalf("len(DrainHooks) = %d, want 1", len(b.DrainHooks))
	}
	app := &fakeLynx{}

	b.Apply(app)

	if len(app.onDrains) != 1 {
		t.Fatalf("Apply() registered %d drain hooks, want 1", len(app.onDrains))
	}
	_ = app.onDrains[0](context.Background())
	if !drainRan {
		t.Error("registered drain hook should run")
	}
}
