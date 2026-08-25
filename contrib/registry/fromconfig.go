package registry

import (
	"fmt"
	"strings"
	"time"

	"github.com/lynx-go/lynx"
)

// fileConfig 是 registry 配置段的映射（yaml 键 → mapstructure 字段）。
type fileConfig struct {
	Enabled           *bool             `mapstructure:"enabled"`
	Backend           string            `mapstructure:"backend"`
	FailFast          *bool             `mapstructure:"fail_fast"`
	AffectReadiness   *bool             `mapstructure:"affect_readiness"`
	HeartbeatInterval time.Duration     `mapstructure:"heartbeat_interval"`
	HeartbeatTTL      time.Duration     `mapstructure:"heartbeat_ttl"`
	DeregisterAfter   time.Duration     `mapstructure:"deregister_after"`
	AdvertiseTimeout  time.Duration     `mapstructure:"advertise_timeout"`
	Tags              []string          `mapstructure:"tags"`
	Meta              map[string]string `mapstructure:"meta"`
	Weight            int               `mapstructure:"weight"`
	Advertise         struct {
		Host string `mapstructure:"host"`
	} `mapstructure:"advertise"`
	Endpoints   []Endpoint `mapstructure:"endpoints"`
	ServiceName string     `mapstructure:"service_name"`
	InstanceID  string     `mapstructure:"instance_id"`
	Discovery   struct {
		PollInterval time.Duration `mapstructure:"poll_interval"`
	} `mapstructure:"discovery"`
	DNS struct {
		Domain    string         `mapstructure:"domain"`
		Namespace string         `mapstructure:"namespace"`
		Ports     map[string]int `mapstructure:"ports"`
	} `mapstructure:"dns"`
}

// NewBackendFromConfig 按 registry.backend 构造零依赖后端（memory/dns；
// consul 由应用侧 consul.NewFromConfig 构造）。
//
//   - registry 段缺失 / enabled:false / backend:"" → (nil, nil, nil)；
//   - backend=memory → memory 同时作为 Registry 与 Discovery；
//   - backend=dns → (nil, Discovery, nil)：只读，读 registry.dns.* 与
//     registry.discovery.poll_interval；不要 Bind Registrar；
//   - backend=consul → 明确错误：请使用 consul.NewFromConfig（避免
//     contrib/registry → contrib/consul → contrib/registry 依赖环）；
//   - 未知 backend → error。
func NewBackendFromConfig(cfg lynx.Config) (Registry, Discovery, error) {
	fc, ok, err := loadFileConfig(cfg)
	if err != nil || !ok {
		return nil, nil, err
	}
	switch fc.Backend {
	case "memory":
		mem := NewMemory()
		return mem, mem, nil
	case "dns":
		return nil, newDNSFromConfig(fc), nil
	case "consul":
		return nil, nil, fmt.Errorf("registry: backend consul 请使用 consul.NewFromConfig 构造")
	default:
		return nil, nil, fmt.Errorf("registry: unknown backend %q", fc.Backend)
	}
}

// newDNSFromConfig 用 registry.dns.* 与 registry.discovery.poll_interval
// 构造 DNS Discovery（未设置的字段取默认值）。
func newDNSFromConfig(fc fileConfig) Discovery {
	return NewDNSDiscovery(
		WithDNSDomain(fc.DNS.Domain),
		WithDNSNamespace(fc.DNS.Namespace),
		WithDNSPorts(fc.DNS.Ports),
		WithDNSPollInterval(fc.Discovery.PollInterval),
	)
}

// NewFromConfig 从 registry 段构造 Registrar。约定对齐 kafka.NewFromConfig：
//
//   - registry 段缺失 / enabled:false / backend:"" → (nil, nil)，调用方
//     不得 Register（Bind 已对 nil 做 no-op）；
//   - backend:dns → (nil, nil)（DNS 只读，不要 Registrar）；
//   - 需要写目录的 backend（memory/consul）且 r == nil → error；
//   - 段存在但字段类型非法 → error，由 Run() 暴露。
//
// 没有 registry.command 开关：长期服务 setup 调用 Bind；app.Command /
// 一次性 CLI 的 setup 不要调用 Bind（affect_readiness=true 时 command.go
// 会空等注册中心，且 CLI 会写下一条短命目录）。
//
// 默认 flags 不做 AutomaticEnv/BindEnv：除 LYNX_ADVERTISE_HOST 由
// Registrar 直读外，其它 LYNX_REGISTRY_* 只有应用自己的 BindConfigFunc
// 写了绑定才会生效。可复制片段：
//
//	c.SetEnvPrefix("LYNX")
//	c.AutomaticEnv()
//	_ = c.BindEnv("registry.enabled", "LYNX_REGISTRY_ENABLED")
//	_ = c.BindEnv("registry.backend", "LYNX_REGISTRY_BACKEND")
//	_ = c.BindEnv("registry.consul.address", "LYNX_REGISTRY_CONSUL_ADDRESS")
//	_ = c.BindEnv("registry.consul.token", "LYNX_REGISTRY_CONSUL_TOKEN")
//	// Consul 官方 CONSUL_HTTP_TOKEN 由 contrib/consul 在 New 时 os.Getenv
//	// 直读，优先于配置文件（空 token 才回落配置）。
func NewFromConfig(cfg lynx.Config, r Registry, advertisers ...Advertiser) (*Registrar, error) {
	fc, ok, err := loadFileConfig(cfg)
	if err != nil || !ok {
		return nil, err
	}
	switch fc.Backend {
	case "dns":
		return nil, nil
	case "memory", "consul":
		// 需要写目录，继续向下
	default:
		return nil, fmt.Errorf("registry: unknown backend %q", fc.Backend)
	}
	if r == nil {
		return nil, fmt.Errorf("registry: backend %q requires a Registry", fc.Backend)
	}

	opts := []RegistrarOption{
		WithAdvertisers(advertisers...),
		WithStaticEndpoints(fc.Endpoints...),
		WithAdvertiseHost(fc.Advertise.Host),
		WithTags(fc.Tags...),
		WithMeta(fc.Meta),
		WithServiceName(fc.ServiceName),
		WithInstanceID(fc.InstanceID),
	}
	if fc.FailFast != nil {
		opts = append(opts, WithFailFast(*fc.FailFast))
	}
	if fc.AffectReadiness != nil {
		opts = append(opts, WithAffectReadiness(*fc.AffectReadiness))
	}
	if fc.HeartbeatInterval > 0 {
		opts = append(opts, WithHeartbeatInterval(fc.HeartbeatInterval))
	}
	if fc.AdvertiseTimeout > 0 {
		opts = append(opts, WithAdvertiseTimeout(fc.AdvertiseTimeout))
	}
	if fc.Weight != 0 {
		opts = append(opts, WithWeight(fc.Weight))
	}
	// heartbeat_ttl / deregister_after 不进 Registrar：TTL 后端（consul）
	// 由 consul.NewFromConfig 自行读同一配置段（registry.heartbeat_ttl /
	// registry.deregister_after），Registrar 没有传递通道，保留字段只会
	// 制造「已生效」的假象（RC-06）。此处只做交叉校验：interval ≥ TTL 时
	// TTL 必然在两次心跳之间过期，实例在目录里持续闪断且无任何告警，
	// 属配置错误，直接失败。
	if fc.HeartbeatInterval > 0 && fc.HeartbeatTTL > 0 && fc.HeartbeatInterval >= fc.HeartbeatTTL {
		return nil, fmt.Errorf("registry: heartbeat_interval (%s) must be < heartbeat_ttl (%s): "+
			"TTL 会在两次心跳之间过期，实例将持续闪断", fc.HeartbeatInterval, fc.HeartbeatTTL)
	}
	reg := NewRegistrar(r, opts...)
	return reg, nil
}

// Bind 是推荐入口：Register 服务 + 挂 OnDrain 注销钩子。r == nil 时
// no-op。通过 type-assert interface{ OnDrain(...) } 调用，以便老测试
// fake 未实现该方法时仍能编译（只 Register，不挂钩）；生产 go.mod 仍
// require 含 OnDrain 的根版本。
func Bind(app lynx.App, r *Registrar) {
	if app == nil || r == nil {
		return
	}
	app.Register(r)
	if d, ok := app.(interface{ OnDrain(fns ...lynx.HookFunc) }); ok {
		d.OnDrain(r.DeregisterHook())
	}
}

// loadFileConfig 读取 registry 段。返回 ok=false 表示未启用（段缺失 /
// enabled:false / backend:""），调用方按 (nil, nil) 处理。
func loadFileConfig(cfg lynx.Config) (fileConfig, bool, error) {
	var fc fileConfig
	if cfg.Get("registry") == nil {
		return fc, false, nil
	}
	if err := validateRegistrySection(cfg.Get("registry")); err != nil {
		return fc, false, err
	}
	if err := cfg.UnmarshalKey("registry", &fc); err != nil {
		return fc, false, err
	}
	if fc.Enabled != nil && !*fc.Enabled {
		return fc, false, nil
	}
	if fc.Backend == "" {
		return fc, false, nil
	}
	return fc, true, nil
}

// validateRegistrySection 轻量校验 registry 段的字段结构类型，拒绝
// mapstructure 弱类型转换会静默掩盖的类型错误（对齐 contrib/watermill-kafka）。
// 仅在 cfg.Get 返回映射时生效。值为 nil 的字段（YAML null）视为未设置。
func validateRegistrySection(raw any) error {
	section, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := foldGet(section, "tags"); ok && v != nil && !isStringList(v) {
		return fmt.Errorf("registry: field tags must be a list of strings, got %T", v)
	}
	if v, ok := foldGet(section, "meta"); ok && v != nil {
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("registry: field meta must be a mapping, got %T", v)
		}
	}
	if v, ok := foldGet(section, "endpoints"); ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("registry: field endpoints must be a list, got %T", v)
		}
		for i, item := range list {
			if _, ok := item.(map[string]any); !ok {
				return fmt.Errorf("registry: endpoints[%d] must be a mapping, got %T", i, item)
			}
		}
	}
	return nil
}

// isStringList 判断值是否为字符串列表；字符串本身也接受（env 注入的
// 扁平值或单值简写，弱类型转换下行为不变）。
func isStringList(v any) bool {
	switch v.(type) {
	case []any, []string, string:
		return true
	}
	return false
}

// foldGet 大小写不敏感地从映射取键（viper 对配置键大小写不敏感）。
func foldGet(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}
