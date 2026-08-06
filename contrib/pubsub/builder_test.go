package pubsub

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

func builderTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return lynx.NewViperConfig(v)
}

// TestNewFromConfigRoutesApplied 验证显式路由与 key 别名生效。
func TestNewFromConfigRoutesApplied(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
      key: hello
    notify:
      transport: memory
      key: user_notify
`), map[string]Transport{"kafka": kafkaT, "memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b.Broker == nil {
		t.Fatal("expected non-nil broker")
	}
	if err := b.Broker.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
	if err := b.Broker.Publish(t.Context(), "notify", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish notify: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "user_notify" {
		t.Fatalf("notify published to %v, want [user_notify]", got)
	}
}

// TestNewFromConfigUnknownTransport 验证未知 transport 标识报错。
func TestNewFromConfigUnknownTransport(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: redis
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "redis"`) {
		t.Fatalf("expected unknown transport error, got %v", err)
	}
}

// TestNewFromConfigKafkaDisabledRouteError 验证 kafka 未启用时路由引用 kafka 报错。
func TestNewFromConfigKafkaDisabledRouteError(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "kafka"`) {
		t.Fatalf("expected unknown transport error for disabled kafka, got %v", err)
	}
}

// TestNewFromConfigDefaultMemory 验证无显式路由时未声明 topic 回退内置内存。
func TestNewFromConfigDefaultMemory(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"), map[string]Transport{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(b.Transports) != 1 {
		t.Fatalf("expected 1 built-in memory transport, got %d", len(b.Transports))
	}
	if _, ok := b.Transports[0].(*MemoryTransport); !ok {
		t.Fatalf("expected built-in *MemoryTransport, got %T", b.Transports[0])
	}
	if err := b.Broker.Publish(t.Context(), "anything", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish to default memory: %v", err)
	}
}

// TestNewFromConfigProvidedMemory 验证调用方提供的 memory 被复用，不重复创建。
func TestNewFromConfigProvidedMemory(t *testing.T) {
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(b.Transports) != 1 || b.Transports[0] != memT {
		t.Fatalf("expected provided memory transport reused, got %v", b.Transports)
	}
	if err := b.Broker.Publish(t.Context(), "anything", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish to provided memory: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "anything" {
		t.Fatalf("published to %v, want [anything]", got)
	}
}

// TestNewFromConfigNilEntrySkipped 验证字面 nil 条目被防御性跳过。
func TestNewFromConfigNilEntrySkipped(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": nil})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(b.Transports) != 1 {
		t.Fatalf("expected only built-in memory transport, got %d", len(b.Transports))
	}
}

// TestNewFromConfigComponentsOrder 验证 Components() 顺序稳定（transports 前、broker 后）。
func TestNewFromConfigComponentsOrder(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	comps := b.Components()
	if len(comps) != len(b.Transports)+1 {
		t.Fatalf("Components len %d, want %d", len(comps), len(b.Transports)+1)
	}
	for i, tr := range b.Transports {
		if comps[i] != tr {
			t.Fatalf("Components[%d] = %v, want transport %v", i, comps[i], tr)
		}
	}
	if comps[len(comps)-1] != b.Broker {
		t.Fatalf("last component = %v, want broker", comps[len(comps)-1])
	}
}
