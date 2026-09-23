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

// 默认语义（v1.12 起结构体驱动逐叶取值转正，原 WithEnvForAllKeys 删除）：
// json tag 参与回退链、env-only 键可见、mapstructure 优先于 json。
func TestUnmarshalDefaultStructDriven(t *testing.T) {
	t.Setenv("PROBE_MAX_IDLE", "5")
	t.Setenv("PROBE_PLAIN", "plain-env")
	t.Setenv("PROBE_BOTH_MS", "ms-env")
	t.Setenv("PROBE_ONLY_MS", "only-ms-env")
	t.Setenv("PROBE_CHILDREN", "a,b")
	t.Setenv("PROBE_DUR", "5s")
	c := newUnmarshalTestConfig(t, "PROBE")

	var p unmarshalProbe
	if err := c.Unmarshal(&p); err != nil {
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

func TestUnmarshalWithTagName(t *testing.T) {
	t.Setenv("PROBE_BOTH_JSON", "json-env")
	c := newUnmarshalTestConfig(t, "PROBE")

	var p unmarshalProbe
	if err := c.Unmarshal(&p, WithTagName("json")); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Both != "json-env" {
		t.Errorf("Both = %q, want json-env（TagName 限定 json）", p.Both)
	}
}

func TestUnmarshalKeyStructDriven(t *testing.T) {
	t.Setenv("PROBE_KAFKA_ORDERS_GROUP_ID", "env-group")
	c := newUnmarshalTestConfig(t, "PROBE")

	var got struct {
		Orders struct {
			Brokers []string `json:"brokers"`
			GroupID string   `json:"group_id"`
		} `json:"orders"`
	}
	if err := c.UnmarshalKey("kafka", &got); err != nil {
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
func TestUnmarshalNonStructFallsBackToViper(t *testing.T) {
	c := newUnmarshalTestConfig(t, "PROBE")

	var m map[string]any
	if err := c.Unmarshal(&m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m["addr"] != ":9090" {
		t.Errorf("m[addr] = %v, want :9090", m["addr"])
	}
}

// TestUnmarshalNestedContainerUsesMapstructureTags 回归：容器字段
// （map/切片且元素为结构体，含 remain map）的元素键是真实配置键
// （如 max_redeliveries），必须按 mapstructure 语义解码——按字段名匹配
// 会静默解出零值（watermill bus 段曾踩中）。
func TestUnmarshalNestedContainerUsesMapstructureTags(t *testing.T) {
	c := newStrictTestConfig(t, `
bus:
  topics:
    order-events:
      group: g
      max_redeliveries: 2
`)
	var out struct {
		Topics map[string]struct {
			Group           string `mapstructure:"group"`
			MaxRedeliveries int    `mapstructure:"max_redeliveries"`
		} `mapstructure:"topics"`
	}
	if err := c.UnmarshalKey("bus", &out); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	tc, ok := out.Topics["order-events"]
	if !ok {
		t.Fatalf("topics = %v, want order-events", out.Topics)
	}
	if tc.Group != "g" {
		t.Errorf("Group = %q, want g", tc.Group)
	}
	if tc.MaxRedeliveries != 2 {
		t.Errorf("MaxRedeliveries = %d, want 2（嵌套下划线键不得静默解成零值）", tc.MaxRedeliveries)
	}
}

// TestUnmarshalRemainMapDecodesNestedTags 回归：remain map（kafka 的
// map[逻辑topic]TopicOptions 形态）整体取子树，嵌套字段同样按
// mapstructure 语义解码。
func TestUnmarshalRemainMapDecodesNestedTags(t *testing.T) {
	c := newStrictTestConfig(t, `
kafka:
  orders:
    brokers: ["b1"]
    consumer:
      group_id: env-group
`)
	var out struct {
		Topics map[string]struct {
			Brokers  []string `mapstructure:"brokers"`
			Consumer *struct {
				GroupID string `mapstructure:"group_id"`
			} `mapstructure:"consumer"`
		} `mapstructure:",remain"`
	}
	if err := c.UnmarshalKey("kafka", &out); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	tc, ok := out.Topics["orders"]
	if !ok || len(tc.Brokers) != 1 || tc.Brokers[0] != "b1" {
		t.Fatalf("orders = %+v, want brokers [b1]", out.Topics)
	}
	if tc.Consumer == nil || tc.Consumer.GroupID != "env-group" {
		t.Fatalf("consumer = %+v, want group_id env-group", tc.Consumer)
	}
}
// newStrictTestConfig 构造带类型错配场景的配置。
func newStrictTestConfig(t *testing.T, yaml string) Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return NewViperConfig(v)
}

// TestUnmarshalStrictTypes 钉住严格类型的精确语义：拒绝非字符串标量到
// 集合的弱转；字符串来源（env 注入、单值简写）与标量间弱转不受影响。
func TestUnmarshalStrictTypes(t *testing.T) {
	t.Run("scalar-to-slice rejected", func(t *testing.T) {
		c := newStrictTestConfig(t, `
kafka:
  orders:
    brokers: 42
`)
		var out struct {
			Orders struct {
				Brokers []string `mapstructure:"brokers"`
			} `mapstructure:"orders"`
		}
		err := c.UnmarshalKey("kafka", &out, WithStrictTypes())
		if err == nil {
			t.Fatal("want error for brokers: 42 (int) → []string")
		}
	})

	t.Run("scalar-to-map rejected", func(t *testing.T) {
		c := newStrictTestConfig(t, `
registry:
  meta: 42
`)
		var out struct {
			Meta map[string]string `mapstructure:"meta"`
		}
		err := c.UnmarshalKey("registry", &out, WithStrictTypes())
		if err == nil {
			t.Fatal("want error for meta: 42 (int) → map")
		}
	})

	t.Run("string-list-still-accepted", func(t *testing.T) {
		c := newStrictTestConfig(t, `
kafka:
  orders:
    brokers: "b1,b2"
`)
		var out struct {
			Orders struct {
				Brokers []string `mapstructure:"brokers"`
			} `mapstructure:"orders"`
		}
		if err := c.UnmarshalKey("kafka", &out, WithStrictTypes()); err != nil {
			t.Fatalf("string source must stay legal (env 注入恒为字符串): %v", err)
		}
		if len(out.Orders.Brokers) != 2 || out.Orders.Brokers[0] != "b1" {
			t.Fatalf("Brokers = %v, want [b1 b2]", out.Orders.Brokers)
		}
	})

	t.Run("env-string-to-int-still-accepted", func(t *testing.T) {
		c := newUnmarshalTestConfig(t, "STRICT")
		t.Setenv("STRICT_KAFKA_ORDERS_MAX", "7")
		var out struct {
			Orders struct {
				Max int `mapstructure:"max"`
			} `mapstructure:"orders"`
		}
		if err := c.UnmarshalKey("kafka", &out, WithStrictTypes()); err != nil {
			t.Fatalf("env string → int must stay legal: %v", err)
		}
		if out.Orders.Max != 7 {
			t.Fatalf("Max = %d, want 7", out.Orders.Max)
		}
	})

	t.Run("weak-conversion-kept-without-option", func(t *testing.T) {
		c := newStrictTestConfig(t, `
kafka:
  orders:
    brokers: 42
`)
		var out struct {
			Orders struct {
				Brokers []string `mapstructure:"brokers"`
			} `mapstructure:"orders"`
		}
		if err := c.UnmarshalKey("kafka", &out); err != nil {
			t.Fatalf("without option old weak behavior must hold: %v", err)
		}
		if len(out.Orders.Brokers) != 1 || out.Orders.Brokers[0] != "42" {
			t.Fatalf("Brokers = %v, want [42]（旧行为：静默弱转）", out.Orders.Brokers)
		}
	})
}

// TestUnmarshalKeyErrorUnused 钉住未知键检测：未消费键报错、动态 map
// 整体消费、Unmarshal 不支持时明确报错。
func TestUnmarshalKeyErrorUnused(t *testing.T) {
	t.Run("unknown-key-reported", func(t *testing.T) {
		c := newStrictTestConfig(t, `
kafka:
  orders:
    brokers: ["b1"]
    broker_typo: ["b2"]
`)
		var out struct {
			Orders struct {
				Brokers []string `mapstructure:"brokers"`
			} `mapstructure:"orders"`
		}
		err := c.UnmarshalKey("kafka", &out, WithErrorUnused())
		if err == nil || !strings.Contains(err.Error(), "broker_typo") {
			t.Fatalf("want error naming broker_typo, got: %v", err)
		}
	})

	t.Run("dynamic-map-consumes-all", func(t *testing.T) {
		c := newStrictTestConfig(t, `
kafka:
  anything:
    brokers: ["b1"]
`)
		var out struct {
			Topics map[string]struct {
				Brokers []string `mapstructure:"brokers"`
			} `mapstructure:",remain"`
		}
		if err := c.UnmarshalKey("kafka", &out, WithErrorUnused()); err != nil {
			t.Fatalf("dynamic map field must consume whole subtree: %v", err)
		}
	})

	t.Run("unmarshal-rejects-option", func(t *testing.T) {
		c := newStrictTestConfig(t, "addr: :9090")
		var out struct {
			Addr string `mapstructure:"addr"`
		}
		err := c.Unmarshal(&out, WithErrorUnused())
		if err == nil || !strings.Contains(err.Error(), "UnmarshalKey") {
			t.Fatalf("want explicit unsupported error, got: %v", err)
		}
	})

	t.Run("nested-struct-descends", func(t *testing.T) {
		c := newStrictTestConfig(t, `
kafka:
  orders:
    brokers: ["b1"]
    consumer:
      group_id: g
      group_idd: typo
`)
		var out struct {
			Orders struct {
				Brokers  []string `mapstructure:"brokers"`
				Consumer struct {
					GroupID string `mapstructure:"group_id"`
				} `mapstructure:"consumer"`
			} `mapstructure:"orders"`
		}
		err := c.UnmarshalKey("kafka", &out, WithErrorUnused())
		if err == nil || !strings.Contains(err.Error(), "group_idd") {
			t.Fatalf("want error naming nested group_idd, got: %v", err)
		}
	})
}
