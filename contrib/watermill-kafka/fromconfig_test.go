package kafka

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

func fromConfigTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return lynx.NewViperConfig(v)
}

func TestNewFromConfigMissingSection(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, "addr: \":9090\"\n"))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if tr != nil {
		t.Fatalf("expected nil transport for missing kafka section, got %+v", tr)
	}
}

func TestNewFromConfigEmptySection(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, "kafka: {}\n"))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if tr != nil {
		t.Fatalf("expected nil transport for empty kafka section, got %+v", tr)
	}
}

func TestNewFromConfigValid(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, `
kafka:
  hello:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_hello]
    consumer:
      group_id: consumer_hello
    producer:
      required_acks: -1
`))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport for valid kafka section")
	}
	topics := tr.Topics()
	if len(topics) != 1 || topics[0] != "hello" {
		t.Fatalf("unexpected topics: %v", topics)
	}
}

// TestNewFromConfigInvalidSection 验证 kafka 段类型非法时返回错误。
func TestNewFromConfigInvalidSection(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, `
kafka:
  hello:
    brokers: 42
`))
	if err == nil {
		t.Fatal("expected error for invalid kafka section (brokers must be []string)")
	}
	if tr != nil {
		t.Fatalf("expected nil transport on error, got %+v", tr)
	}
}

// TestUnmarshalStrictTypes 表驱动覆盖严格类型语义的全部分支（原手写
// validateKafkaSection 垫片的行为矩阵，现由 lynx.WithStrictTypes 统一实现）：
// YAML null 视为未设置不报错；标量/错误类型被拒；字符串来源（env/简写）
// 接受；键大小写不敏感。
func TestUnmarshalStrictTypes(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"brokers null treated as absent", "kafka:\n  hello:\n    brokers: null\n", false},
		{"brokers scalar int rejected", "kafka:\n  hello:\n    brokers: 42\n", true},
		{"topics scalar int rejected", "kafka:\n  hello:\n    topics: 42\n", true},
		{"topic value not a mapping rejected", "kafka:\n  hello: hello\n", true},
		{"consumer scalar rejected", "kafka:\n  hello:\n    consumer: 42\n", true},
		{"sasl null treated as absent", "kafka:\n  hello:\n    sasl: null\n", false},
		{"brokers scalar string accepted", "kafka:\n  hello:\n    brokers: \"127.0.0.1:19092\"\n", false},
		{"scalar section rejected at decode", "kafka: true\n", true},
		{"case-variant keys accepted", "kafka:\n  hello:\n    Brokers: [x]\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts Options
			err := fromConfigTestConfig(t, tt.yaml).UnmarshalKey("kafka", &opts, lynx.WithStrictTypes())
			if tt.wantErr && err == nil {
				t.Fatal("expected decode error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no decode error, got %v", err)
			}
		})
	}
}
