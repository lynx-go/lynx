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
