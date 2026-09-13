package lynx

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

type unmarshalSub struct {
	Token string `json:"token"`
}

type unmarshalProbe struct {
	Addr     string `mapstructure:"addr" json:"addr"`
	MaxIdle  int    `json:"max_idle"`
	Both     string `mapstructure:"both_ms" json:"both_json"`
	OnlyMS   string `mapstructure:"only_ms"`
	Plain    string
	Sub      unmarshalSub  `json:"sub"`
	SubPtr   *unmarshalSub `json:"sub_ptr"`
	Dur      time.Duration `json:"dur"`
	Children []string      `json:"children"`
	hidden   string
}

func newUnmarshalTestConfig(t *testing.T, envPrefix string) ConfigSource {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
addr: ":9090"
sub:
  token: file-token
kafka:
  orders:
    brokers: ["127.0.0.1:19092"]
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	c := NewViperConfig(v)
	c.SetEnvPrefix(envPrefix)
	c.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	c.AutomaticEnv()
	return c
}

// 默认路径保持 viper 语义：json tag 不参与匹配，env-only 键不可见。
func TestUnmarshalDefaultViperSemantics(t *testing.T) {
	t.Setenv("PROBE_MAX_IDLE", "5")
	c := newUnmarshalTestConfig(t, "PROBE")

	var p unmarshalProbe
	if err := c.Unmarshal(&p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", p.Addr)
	}
	if p.MaxIdle != 0 {
		t.Errorf("MaxIdle = %d, want 0（json tag 不参与默认匹配，env-only 键不可见）", p.MaxIdle)
	}
}

func TestUnmarshalEnvForAllKeys(t *testing.T) {
	t.Setenv("PROBE_MAX_IDLE", "5")
	t.Setenv("PROBE_PLAIN", "plain-env")
	t.Setenv("PROBE_BOTH_MS", "ms-env")
	t.Setenv("PROBE_ONLY_MS", "only-ms-env")
	t.Setenv("PROBE_CHILDREN", "a,b")
	t.Setenv("PROBE_DUR", "5s")
	c := newUnmarshalTestConfig(t, "PROBE")

	var p unmarshalProbe
	if err := c.Unmarshal(&p, WithEnvForAllKeys()); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", p.Addr)
	}
	if p.MaxIdle != 5 {
		t.Errorf("MaxIdle = %d, want 5（json tag + env-only 键）", p.MaxIdle)
	}
	if p.Plain != "plain-env" {
		t.Errorf("Plain = %q, want plain-env（无 tag 回退小写字段名）", p.Plain)
	}
	if p.Both != "ms-env" {
		t.Errorf("Both = %q, want ms-env（mapstructure 优先于 json）", p.Both)
	}
	if p.OnlyMS != "only-ms-env" {
		t.Errorf("OnlyMS = %q, want only-ms-env", p.OnlyMS)
	}
	if p.Sub.Token != "file-token" {
		t.Errorf("Sub.Token = %q, want file-token", p.Sub.Token)
	}
	if p.Children == nil || len(p.Children) != 2 || p.Children[0] != "a" || p.Children[1] != "b" {
		t.Errorf("Children = %v, want [a b]", p.Children)
	}
	if p.Dur != 5*time.Second {
		t.Errorf("Dur = %v, want 5s", p.Dur)
	}
	if p.SubPtr != nil {
		t.Errorf("SubPtr = %+v, want nil（未配置的子树保持零值）", p.SubPtr)
	}
	if p.hidden != "" {
		t.Errorf("hidden = %q, want 空（未导出字段不参与）", p.hidden)
	}
}

func TestUnmarshalEnvForAllKeysWithTagName(t *testing.T) {
	t.Setenv("PROBE_BOTH_JSON", "json-env")
	c := newUnmarshalTestConfig(t, "PROBE")

	var p unmarshalProbe
	if err := c.Unmarshal(&p, WithEnvForAllKeys(), WithTagName("json")); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Both != "json-env" {
		t.Errorf("Both = %q, want json-env（TagName 限定 json）", p.Both)
	}
}

func TestUnmarshalKeyEnvForAllKeys(t *testing.T) {
	t.Setenv("PROBE_KAFKA_ORDERS_GROUP_ID", "env-group")
	c := newUnmarshalTestConfig(t, "PROBE")

	var got struct {
		Orders struct {
			Brokers []string `json:"brokers"`
			GroupID string   `json:"group_id"`
		} `json:"orders"`
	}
	if err := c.UnmarshalKey("kafka", &got, WithEnvForAllKeys()); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	if len(got.Orders.Brokers) != 1 || got.Orders.Brokers[0] != "127.0.0.1:19092" {
		t.Fatalf("Brokers = %v, want 文件值", got.Orders.Brokers)
	}
	if got.Orders.GroupID != "env-group" {
		t.Errorf("GroupID = %q, want env-group（叶子键以 path 为前缀）", got.Orders.GroupID)
	}
}

// 非结构体目标无法枚举叶子，回落 viper 路径（文件键照常解码）。
func TestUnmarshalEnvForAllKeysNonStructFallback(t *testing.T) {
	c := newUnmarshalTestConfig(t, "PROBE")

	var m map[string]any
	if err := c.Unmarshal(&m, WithEnvForAllKeys()); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m["addr"] != ":9090" {
		t.Errorf("m[addr] = %v, want :9090", m["addr"])
	}
}
