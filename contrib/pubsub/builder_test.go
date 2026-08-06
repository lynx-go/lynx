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

// TestNewFromConfigRouteKeyDefaultsToTopic 验证路由未指定 key 时缺省为逻辑 topic 名。
func TestNewFromConfigRouteKeyDefaultsToTopic(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
`), map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Broker.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
}

// TestNewFromConfigRouteToBuiltinMemory 验证显式路由指向未提供的 memory
// 标识时解析到内置内存 Transport（不报未知标识错误）。
func TestNewFromConfigRouteToBuiltinMemory(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    notify:
      transport: memory
`), map[string]Transport{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Broker.Publish(t.Context(), "notify", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish notify via built-in memory: %v", err)
	}
}

// TestNewFromConfigComponentsOrder 验证 Components() 顺序确定性：按名字
// 排序的 transports（内置 memory 最后）在前、Broker 最后。
func TestNewFromConfigComponentsOrder(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	want := []lynx.Component{kafkaT, b.Transports[1], b.Broker}
	comps := b.Components()
	if len(comps) != 3 {
		t.Fatalf("Components len %d, want 3", len(comps))
	}
	for i, w := range want {
		if comps[i] != w {
			t.Fatalf("Components[%d] = %v, want %v", i, comps[i], w)
		}
	}
}

// TestNewFromConfigComponentsOrderMulti 验证多传输时 Components() 顺序：
// transports 按名字排序（kafka < redis，与 Transports 列表一致）、内置
// memory 最后追加、Broker 殿后。单传输用例无法区分排序与否，此用例钉住
// 确定性顺序。
func TestNewFromConfigComponentsOrderMulti(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	redisT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT, "redis": redisT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b.Transports[0] != kafkaT || b.Transports[1] != redisT {
		t.Fatalf("Transports[0:2] = %v, %v, want kafkaT, redisT", b.Transports[0], b.Transports[1])
	}
	if _, ok := b.Transports[2].(*MemoryTransport); !ok {
		t.Fatalf("Transports[2] = %T, want built-in *MemoryTransport", b.Transports[2])
	}
	want := []lynx.Component{kafkaT, redisT, b.Transports[2], b.Broker}
	comps := b.Components()
	if len(comps) != 4 {
		t.Fatalf("Components len %d, want 4", len(comps))
	}
	for i, w := range want {
		if comps[i] != w {
			t.Fatalf("Components[%d] = %v, want %v", i, comps[i], w)
		}
	}
}
