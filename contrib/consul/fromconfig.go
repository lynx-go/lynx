package consul

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/lynx-go/lynx"
)

// fileConfig 是 registry 配置段中 consul 关心的子树。
type fileConfig struct {
	Enabled         *bool         `mapstructure:"enabled"`
	Backend         string        `mapstructure:"backend"`
	HeartbeatTTL    time.Duration `mapstructure:"heartbeat_ttl"`
	DeregisterAfter time.Duration `mapstructure:"deregister_after"`
	HealthCheck     struct {
		Type     string        `mapstructure:"type"`
		Path     string        `mapstructure:"path"`
		Interval time.Duration `mapstructure:"interval"`
		Timeout  time.Duration `mapstructure:"timeout"`
	} `mapstructure:"health_check"`
	Consul struct {
		Address    string `mapstructure:"address"`
		Token      string `mapstructure:"token"`
		Datacenter string `mapstructure:"datacenter"`
		Namespace  string `mapstructure:"namespace"`
		AllowStale bool   `mapstructure:"allow_stale"`
		TLS        struct {
			Enabled            bool   `mapstructure:"enabled"`
			CAFile             string `mapstructure:"ca_file"`
			CertFile           string `mapstructure:"cert_file"`
			KeyFile            string `mapstructure:"key_file"`
			InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
		} `mapstructure:"tls"`
	} `mapstructure:"consul"`
}

// NewFromConfig 从 registry.consul 段构造 Client。
//
//   - registry 段缺失 / enabled:false / backend 非 consul → (nil, nil)；
//   - token 为空时直读 CONSUL_HTTP_TOKEN（优先于配置文件）；
//   - TLS 一等配置：registry.consul.tls.{enabled,ca_file,cert_file,
//     key_file,insecure_skip_verify}，默认关（与 gRPC 客户端默认
//     insecure 同一诚实态度），生产应开启；
//   - address 支持裸 host:port 或带 scheme 的 URL（如
//     http://127.0.0.1:8500，测试用）。
//
// 由应用 setup 调用（registry.NewBackendFromConfig 对 backend=consul
// 会返回指向本函数的错误），不要 Bind 到 CLI 一次性命令。
func NewFromConfig(cfg lynx.Config) (*Client, error) {
	if cfg.Get("registry") == nil {
		return nil, nil
	}
	var fc fileConfig
	if err := cfg.UnmarshalKey("registry", &fc); err != nil {
		return nil, err
	}
	if fc.Enabled != nil && !*fc.Enabled {
		return nil, nil
	}
	if fc.Backend != "consul" {
		return nil, nil
	}

	apiConfig := api.DefaultConfig()
	if fc.Consul.Address != "" {
		address, scheme, err := parseAddress(fc.Consul.Address)
		if err != nil {
			return nil, err
		}
		// 显式 scheme 与 tls.enabled 冲突时报错而非静默覆盖（RC-19）：
		// 此前 address 写入的 http 会被 tls.enabled 的 https 悄悄改掉，
		// 用户以为走 TLS 明文探测、或以为走明文实际握手失败，均难排障。
		if fc.Consul.TLS.Enabled && scheme == "http" {
			return nil, fmt.Errorf("consul: registry.consul.address %q 显式 http 与 tls.enabled=true 冲突：TLS 应使用 https:// 地址或去掉 scheme",
				fc.Consul.Address)
		}
		apiConfig.Address = address
		if scheme != "" {
			apiConfig.Scheme = scheme
		}
	}
	apiConfig.Token = fc.Consul.Token // 空时 New() 直读 CONSUL_HTTP_TOKEN
	apiConfig.Datacenter = fc.Consul.Datacenter
	apiConfig.Namespace = fc.Consul.Namespace
	if fc.Consul.TLS.Enabled {
		apiConfig.Scheme = "https"
		apiConfig.TLSConfig = api.TLSConfig{
			CAFile:             fc.Consul.TLS.CAFile,
			CertFile:           fc.Consul.TLS.CertFile,
			KeyFile:            fc.Consul.TLS.KeyFile,
			InsecureSkipVerify: fc.Consul.TLS.InsecureSkipVerify,
		}
	}

	return New(apiConfig,
		WithCheckType(fc.HealthCheck.Type),
		WithCheckPath(fc.HealthCheck.Path),
		WithCheckInterval(fc.HealthCheck.Interval),
		WithCheckTimeout(fc.HealthCheck.Timeout),
		WithHeartbeatTTL(fc.HeartbeatTTL),
		WithDeregisterAfter(fc.DeregisterAfter),
		WithAllowStale(fc.Consul.AllowStale),
	)
}

// parseAddress 接受 host:port 或带 scheme 的 URL。
func parseAddress(address string) (host, scheme string, err error) {
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" {
			return "", "", fmt.Errorf("consul: invalid address %q", address)
		}
		return u.Host, u.Scheme, nil
	}
	return address, "", nil
}
