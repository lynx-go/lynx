package registry

import (
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
	// RC-06：interval ≥ TTL 交叉校验——TTL 会在两次心跳间过期，实例持续
	// 闪断，必须在构造期报错。默认值（10s/30s）在合法范围。
	t.Run("heartbeat interval ttl cross validation", func(t *testing.T) {
		cases := []struct {
			name    string
			section map[string]any
			wantErr bool
		}{
			{"interval exceeds ttl", map[string]any{"backend": "memory", "heartbeat_interval": "31s", "heartbeat_ttl": "30s"}, true},
			{"interval equals ttl", map[string]any{"backend": "memory", "heartbeat_interval": "30s", "heartbeat_ttl": "30s"}, true},
			{"interval below ttl", map[string]any{"backend": "memory", "heartbeat_interval": "10s", "heartbeat_ttl": "30s"}, false},
			{"ttl unset skips check", map[string]any{"backend": "memory", "heartbeat_interval": "5s"}, false},
			{"interval unset skips check", map[string]any{"backend": "memory", "heartbeat_ttl": "1s"}, false},
		}
		for _, c := range cases {
			r, err := NewFromConfig(configOf(t, c.section), NewMemory())
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), "heartbeat_interval") {
					t.Fatalf("%s: want mismatch error, got %v", c.name, err)
				}
				continue
			}
			if err != nil || r == nil {
				t.Fatalf("%s: want success, got %v %v", c.name, r, err)
			}
		}
	})
}

// fakeApp 已随 registry.Apply 删除：DrainHook 由根框架在 Register 登记
// 时自动挂载（lynx.Drainer），入口收敛为 app.Register（见 drain_test.go
// 的根包测试与 TestRegistrarDrainHook 的本包测试）。
