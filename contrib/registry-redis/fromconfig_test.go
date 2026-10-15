package registryredis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

// redisConfig 用内存 viper 构造只含 registry 段的配置。
func redisConfig(t *testing.T, section any) lynx.Config {
	t.Helper()
	v := viper.New()
	if section != nil {
		v.Set("registry", section)
	}
	return lynx.NewViperConfig(v)
}

// TestNewFromConfigDisabled：段缺失 / enabled:false / backend 非 redis 时
// 返回 (nil, nil)，调用方按未配置处理。
func TestNewFromConfigDisabled(t *testing.T) {
	for name, section := range map[string]any{
		"section missing": nil,
		"disabled":        map[string]any{"enabled": false, "backend": "redis"},
		"other backend":   map[string]any{"enabled": true, "backend": "memory"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := NewFromConfig(redisConfig(t, section))
			if err != nil || c != nil {
				t.Fatalf("want (nil,nil), got (%v, %v)", c, err)
			}
		})
	}
}

// TestNewFromConfigApplies：key_prefix 与 heartbeat_ttl 从配置落到客户端；
// 连接归客户端所有（Close 后 ownCloser 生效、再操作返回 ErrClosed）。
func TestNewFromConfigApplies(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := NewFromConfig(redisConfig(t, map[string]any{
		"enabled":       true,
		"backend":       "redis",
		"heartbeat_ttl": 2 * time.Second,
		"redis": map[string]any{
			"address":    mr.Addr(),
			"key_prefix": "app-a/",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Register(context.Background(), inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("app-a/instance/svc/i1") {
		t.Fatal("instance key not written under configured key_prefix")
	}
	if got := mr.TTL("app-a/instance/svc/i1"); got <= 0 || got > 2*time.Second {
		t.Fatalf("ttl = %v, want (0, 2s]", got)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(context.Background(), inst("svc", "i2")); err == nil {
		t.Fatal("Register after Close should fail")
	}
}

// TestNewFromConfigDefaults：redis 子树整体缺省时键走默认前缀、TTL 回落
// 默认 30s、地址回落 127.0.0.1:6379（连接惰性建立，不在此拨号）。
func TestNewFromConfigDefaults(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := NewFromConfig(redisConfig(t, map[string]any{
		"enabled": true,
		"backend": "redis",
		"redis":   map[string]any{"address": mr.Addr()},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Register(context.Background(), inst("svc", "i1")); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("lynx/registry/instance/svc/i1") {
		t.Fatal("instance key not written under default key_prefix")
	}
	if got := mr.TTL("lynx/registry/instance/svc/i1"); got <= 0 || got > 30*time.Second {
		t.Fatalf("ttl = %v, want (0, 30s]", got)
	}
}

// TestNewFromConfigStrictTypes：字段类型非法（db 为字符串）在解码期报错，
// 而不是静默弱转。
func TestNewFromConfigStrictTypes(t *testing.T) {
	_, err := NewFromConfig(redisConfig(t, map[string]any{
		"enabled": true,
		"backend": "redis",
		"redis":   map[string]any{"address": "127.0.0.1:6379", "db": "abc"},
	}))
	if err == nil {
		t.Fatal("want decode error for non-integer db")
	}
}
