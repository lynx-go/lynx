package registry

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

// configOf 用 viper 构造只含给定 registry 段的 lynx.Config。
func configOf(t *testing.T, section map[string]any) lynx.Config {
	t.Helper()
	v := viper.New()
	if section != nil {
		v.Set("registry", section)
	}
	return lynx.NewViperConfig(v)
}

func TestNewBackendFromConfig(t *testing.T) {
	t.Run("section missing", func(t *testing.T) {
		reg, disc, err := NewBackendFromConfig(configOf(t, nil))
		if err != nil || reg != nil || disc != nil {
			t.Fatalf("want (nil,nil,nil), got %v %v %v", reg, disc, err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		reg, disc, err := NewBackendFromConfig(configOf(t, map[string]any{"enabled": false, "backend": "memory"}))
		if err != nil || reg != nil || disc != nil {
			t.Fatalf("want (nil,nil,nil), got %v %v %v", reg, disc, err)
		}
	})
	t.Run("empty backend", func(t *testing.T) {
		reg, disc, err := NewBackendFromConfig(configOf(t, map[string]any{"enabled": true}))
		if err != nil || reg != nil || disc != nil {
			t.Fatalf("want (nil,nil,nil), got %v %v %v", reg, disc, err)
		}
	})
	t.Run("memory", func(t *testing.T) {
		reg, disc, err := NewBackendFromConfig(configOf(t, map[string]any{"backend": "memory"}))
		if err != nil {
			t.Fatal(err)
		}
		if reg == nil || disc == nil {
			t.Fatal("memory backend must provide both Registry and Discovery")
		}
	})
	t.Run("dns", func(t *testing.T) {
		reg, disc, err := NewBackendFromConfig(configOf(t, map[string]any{
			"backend":   "dns",
			"dns":       map[string]any{"domain": "example.local", "namespace": "prod", "ports": map[string]any{"http": 18080}},
			"discovery": map[string]any{"poll_interval": "3s"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if reg != nil {
			t.Fatalf("dns is read-only: want nil Registry, got %v", reg)
		}
		d, ok := disc.(*dnsDiscovery)
		if !ok {
			t.Fatalf("want *dnsDiscovery, got %T", disc)
		}
		if d.domain != "example.local" || d.namespace != "prod" ||
			d.ports["http"] != 18080 || d.ports["grpc"] != 9090 || // 端口表在默认值上覆盖
			d.pollInterval != 3*1000*1000*1000 {
			t.Fatalf("dns config not mapped: %+v", d)
		}
	})
	t.Run("dns defaults", func(t *testing.T) {
		_, disc, err := NewBackendFromConfig(configOf(t, map[string]any{"backend": "dns"}))
		if err != nil {
			t.Fatal(err)
		}
		d := disc.(*dnsDiscovery)
		if d.domain != defaultDNSDomain || d.namespace != defaultDNSNamespace ||
			d.pollInterval != defaultDNSPollInterval || d.ports["http"] != 8080 {
			t.Fatalf("dns defaults not applied: %+v", d)
		}
	})
	t.Run("consul rejected", func(t *testing.T) {
		_, _, err := NewBackendFromConfig(configOf(t, map[string]any{"backend": "consul"}))
		if err == nil || !strings.Contains(err.Error(), "consul.NewFromConfig") {
			t.Fatalf("want consul guidance error, got %v", err)
		}
	})
	t.Run("unknown backend", func(t *testing.T) {
		_, _, err := NewBackendFromConfig(configOf(t, map[string]any{"backend": "etcd"}))
		if err == nil || !strings.Contains(err.Error(), "unknown backend") {
			t.Fatalf("want unknown backend error, got %v", err)
		}
	})
}

func TestNewFromConfig(t *testing.T) {
	t.Run("section missing", func(t *testing.T) {
		r, err := NewFromConfig(configOf(t, nil), NewMemory())
		if err != nil || r != nil {
			t.Fatalf("want (nil,nil), got %v %v", r, err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		r, err := NewFromConfig(configOf(t, map[string]any{"enabled": false, "backend": "memory"}), NewMemory())
		if err != nil || r != nil {
			t.Fatalf("want (nil,nil), got %v %v", r, err)
		}
	})
	t.Run("dns is read-only", func(t *testing.T) {
		r, err := NewFromConfig(configOf(t, map[string]any{"backend": "dns"}), nil)
		if err != nil || r != nil {
			t.Fatalf("want (nil,nil), got %v %v", r, err)
		}
	})
	t.Run("memory requires registry", func(t *testing.T) {
		_, err := NewFromConfig(configOf(t, map[string]any{"backend": "memory"}), nil)
		if err == nil || !strings.Contains(err.Error(), "requires a Registry") {
			t.Fatalf("want requires-a-Registry error, got %v", err)
		}
	})
	t.Run("memory ok with defaults", func(t *testing.T) {
		r, err := NewFromConfig(configOf(t, map[string]any{"backend": "memory"}), NewMemory())
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			t.Fatal("want Registrar")
		}
		// 默认值：fail_fast=true / affect_readiness=true / 10s / 5s / weight=100。
		if !r.opts.failFast || !r.opts.affectReadiness ||
			r.opts.heartbeatInterval != defaultHeartbeatInterval ||
			r.opts.advertiseTimeout != defaultAdvertiseTimeout ||
			r.opts.weight != defaultWeight {
			t.Fatalf("defaults not applied: %+v", r.opts)
		}
	})
	t.Run("fields mapped", func(t *testing.T) {
		r, err := NewFromConfig(configOf(t, map[string]any{
			"backend":            "memory",
			"fail_fast":          false,
			"affect_readiness":   false,
			"heartbeat_interval": "2s",
			"advertise_timeout":  "1s",
			"tags":               []string{"api", "internal"},
			"meta":               map[string]any{"region": "cn-east"},
			"weight":             50,
			"advertise":          map[string]any{"host": "10.0.0.1"},
			"endpoints": []any{map[string]any{
				"protocol": "http",
				"address":  ":8080",
			}},
			"service_name": "cfg-svc",
			"instance_id":  "cfg-i1",
		}), NewMemory())
		if err != nil {
			t.Fatal(err)
		}
		if r.opts.failFast || r.opts.affectReadiness ||
			r.opts.heartbeatInterval != 2*1000*1000*1000 ||
			r.opts.advertiseTimeout != 1*1000*1000*1000 ||
			r.opts.weight != 50 ||
			r.opts.advertiseHost != "10.0.0.1" ||
			r.opts.serviceName != "cfg-svc" || r.opts.instanceID != "cfg-i1" ||
			len(r.opts.tags) != 2 || r.opts.meta["region"] != "cn-east" ||
			len(r.opts.endpoints) != 1 || r.opts.endpoints[0].Address != ":8080" {
			t.Fatalf("fields not mapped: %+v", r.opts)
		}
	})
	t.Run("invalid field types", func(t *testing.T) {
		for name, section := range map[string]map[string]any{
			"tags scalar":      {"backend": "memory", "tags": 42},
			"meta scalar":      {"backend": "memory", "meta": "nope"},
			"endpoints scalar": {"backend": "memory", "endpoints": "http://x"},
			"endpoints item":   {"backend": "memory", "endpoints": []any{42}},
		} {
			if _, err := NewFromConfig(configOf(t, section), NewMemory()); err == nil {
				t.Fatalf("%s: want type error", name)
			}
		}
	})
}

// fakeApp 记录 Register / OnDrain 调用。
type fakeApp struct {
	registered []lynx.Service
	drainHooks []lynx.HookFunc
}

func (f *fakeApp) Context() context.Context       { return context.Background() }
func (f *fakeApp) Config() lynx.Config            { return nil }
func (f *fakeApp) Logger(...any) *slog.Logger     { return slog.Default() }
func (f *fakeApp) HealthCheckers() []lynx.Checker { return nil }
func (f *fakeApp) Close()                         {}
func (f *fakeApp) Command(lynx.CommandFunc) error { return nil }
func (f *fakeApp) OnStart(...lynx.HookFunc)       {}
func (f *fakeApp) OnDrain(fns ...lynx.HookFunc)   { f.drainHooks = append(f.drainHooks, fns...) }
func (f *fakeApp) OnStop(...lynx.HookFunc)        {}
func (f *fakeApp) Register(services ...lynx.Service) {
	f.registered = append(f.registered, services...)
}
func (f *fakeApp) RegisterFactories(...lynx.ServiceFactory) {}
func (f *fakeApp) Run() error                               { return nil }
func (f *fakeApp) SetLogger(*slog.Logger)                   {}

func TestBind(t *testing.T) {
	t.Run("nil registrar is no-op", func(t *testing.T) {
		app := &fakeApp{}
		Bind(app, nil)
		if len(app.registered) != 0 || len(app.drainHooks) != 0 {
			t.Fatalf("Bind(nil) must be no-op, got %+v", app)
		}
	})
	t.Run("registers service and drain hook", func(t *testing.T) {
		app := &fakeApp{}
		r := NewRegistrar(NewMemory(), WithServiceName("svc"))
		Bind(app, r)
		if len(app.registered) != 1 || app.registered[0] != r {
			t.Fatalf("Register not called with registrar: %+v", app.registered)
		}
		if len(app.drainHooks) != 1 {
			t.Fatalf("OnDrain not hooked: %+v", app.drainHooks)
		}
	})
}
