package registryredis

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/redis/go-redis/v9"
)

// defaultAddress 是 registry.redis.address 未配置时的回退地址（与 go-redis
// 客户端默认一致）。
const defaultAddress = "127.0.0.1:6379"

// tlsFileConfig 是 registry.redis.tls 子树：CA 校验服务端（insecure_skip_verify
// 仅测试用，等价 redis-cli --tls --insecure）；cert/key 双向认证按需配置。
type tlsFileConfig struct {
	Enabled            bool   `mapstructure:"enabled"`
	CAFile             string `mapstructure:"ca_file"`
	CertFile           string `mapstructure:"cert_file"`
	KeyFile            string `mapstructure:"key_file"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
}

// fileConfig 是 registry 配置段中 redis 独有的子树。共享字段（enabled /
// backend / heartbeat_ttl / deregister_after）经 registry.LoadFileConfig 读取
// 同一份 schema（FileConfig 唯一归属），不另建副本（RC-06）。
type fileConfig struct {
	Redis struct {
		Address      string        `mapstructure:"address"`
		Username     string        `mapstructure:"username"`
		Password     string        `mapstructure:"password"`
		DB           int           `mapstructure:"db"`
		KeyPrefix    string        `mapstructure:"key_prefix"`
		DialTimeout  time.Duration `mapstructure:"dial_timeout"`
		ReadTimeout  time.Duration `mapstructure:"read_timeout"`
		PoolSize     int           `mapstructure:"pool_size"`
		MinIdleConns int           `mapstructure:"min_idle_conns"`
		TLS          tlsFileConfig `mapstructure:"tls"`
	} `mapstructure:"redis"`
}

// NewFromConfig 从 registry.redis 段构造 Client。
//
//   - registry 段缺失 / enabled:false / backend 非 redis → (nil, nil)；
//   - 连接由本函数自建并归 Client 所有（Close 一并关闭）；需要复用既有
//     连接（Sentinel / Cluster / 连接池共享）时用 New 注入 redis.UniversalClient；
//   - registry.heartbeat_ttl 映射实例键 TTL（默认 30s）；与
//     heartbeat_interval 的交叉校验由 registry.NewFromConfig 承担；
//   - registry.deregister_after 在本后端无意义：TTL 过期即注销（Consul 的
//     DeregisterCriticalServiceAfter 语义已由键过期天然实现），字段被忽略；
//   - 不做启动期 Ping：连接失败由 Registrar 首次 Register（3s 预算）暴露，
//     与 consul.NewFromConfig 的惰性连接同一态度。
//
// 由应用 setup 调用（registry.NewBackendFromConfig 对 backend=redis 会返回
// 指向本函数的错误），随后 registry.NewFromConfig(cfg, c) 构造 Registrar、
// registry.NewResolver(c) 构造发现端。不要 Apply 到 CLI 一次性命令。
func NewFromConfig(cfg lynx.Config) (*Client, error) {
	if cfg.Get("registry") == nil {
		return nil, nil
	}
	shared, ok, err := registry.LoadFileConfig(cfg)
	if err != nil || !ok {
		return nil, err
	}
	if shared.Backend != "redis" {
		return nil, nil
	}
	var fc fileConfig
	if err := cfg.UnmarshalKey("registry", &fc, lynx.WithStrictTypes()); err != nil {
		return nil, err
	}

	opt := &redis.Options{
		Addr:         defaultAddress,
		Username:     fc.Redis.Username,
		Password:     fc.Redis.Password,
		DB:           fc.Redis.DB,
		PoolSize:     fc.Redis.PoolSize,
		MinIdleConns: fc.Redis.MinIdleConns,
	}
	if fc.Redis.Address != "" {
		opt.Addr = fc.Redis.Address
	}
	if fc.Redis.DialTimeout > 0 {
		opt.DialTimeout = fc.Redis.DialTimeout
	}
	if fc.Redis.ReadTimeout > 0 {
		opt.ReadTimeout = fc.Redis.ReadTimeout
	}
	if fc.Redis.TLS.Enabled {
		tlsConfig, err := buildTLSConfig(fc.Redis.TLS)
		if err != nil {
			return nil, err
		}
		opt.TLSConfig = tlsConfig
	}

	rdb := redis.NewClient(opt)
	c := New(rdb,
		WithKeyPrefix(fc.Redis.KeyPrefix),
		WithTTL(shared.HeartbeatTTL),
	)
	c.ownCloser = rdb
	return c, nil
}

// buildTLSConfig 从文件装配客户端 TLS。
func buildTLSConfig(t tlsFileConfig) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // 显式配置项，默认 false
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("registryredis: read ca_file %s: %w", t.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("registryredis: ca_file %s contains no certificate", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("registryredis: load client cert: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
