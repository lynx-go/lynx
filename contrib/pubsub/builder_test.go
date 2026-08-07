package pubsub

import (
	"strings"
	"testing"
	"time"

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

// TestNewFromConfigEventsRouteApplied 验证显式路由（events.<name>.route）
// 与 key 别名生效。
func TestNewFromConfigEventsRouteApplied(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  events:
    hello:
      route:
        transport: kafka
        key: hello
    notify:
      route:
        transport: memory
        key: user_notify
`), map[string]Transport{"kafka": kafkaT, "memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil broker")
	}
	if err := b.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
	if err := b.Publish(t.Context(), "notify", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish notify: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "user_notify" {
		t.Fatalf("notify published to %v, want [user_notify]", got)
	}
}

// TestNewFromConfigRouteKeyDefaultsToTopic 验证路由未指定 key 时缺省为逻辑
// topic 名（fake 不声明 topic，显式路由是唯一路径，独占验证缺省分支）。
func TestNewFromConfigRouteKeyDefaultsToTopic(t *testing.T) {
	kafkaT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  events:
    hello:
      route:
        transport: kafka
`), map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
}

// TestNewFromConfigEventWithoutRouteKeepsOptions 验证无 route 的事件仍保留
// 事件级选项（自动路由/默认回退路径同样生效），且不产生显式路由。
func TestNewFromConfigEventWithoutRouteKeepsOptions(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  events:
    orders:
      log_message: true
      auto_ack: true
      group: orders-group
      instances: 2
      retry:
        max_retries: 0
`), map[string]Transport{"memory": newFakeTransport()})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	// 无 route 声明：显式路由表为空（发布走默认回退而非显式路由报错）。
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  events:
    orders:
      log_message: true
      auto_ack: true
      group: orders-group
      instances: 2
      retry:
        max_retries: 0
`), map[string]Transport{"memory": newFakeTransport()})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	broker := b.(*broker)
	ev, ok := broker.options.Events["orders"]
	if !ok {
		t.Fatal("expected event options for orders")
	}
	if !ev.LogMessage || !ev.AutoAck || ev.Group != "orders-group" || ev.Instances != 2 {
		t.Fatalf("unexpected event options: %+v", ev)
	}
	if ev.Retry == nil || ev.Retry.MaxRetries != 0 {
		t.Fatalf("unexpected event retry: %+v", ev.Retry)
	}
	if len(broker.routes) != 0 {
		t.Fatalf("expected no explicit routes, got %v", broker.routes)
	}
}

// TestNewFromConfigGlobalRetry 验证 pubsub.retry 全局重试配置加载到
// Options.Retry，事件未配置 retry 时作为默认值。
func TestNewFromConfigGlobalRetry(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  retry:
    max_retries: 5
    backoff: 200ms
  events:
    hello:
      route:
        transport: kafka
`), map[string]Transport{"kafka": newFakeTransport()})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	broker := b.(*broker)
	if broker.options.Retry == nil || broker.options.Retry.MaxRetries != 5 || broker.options.Retry.Backoff != 200*time.Millisecond {
		t.Fatalf("unexpected global retry: %+v", broker.options.Retry)
	}
	if got := broker.retryFor("hello"); got.MaxRetries != 5 || got.Backoff != 200*time.Millisecond {
		t.Fatalf("retryFor fallback: %+v", got)
	}
	if got := broker.retryFor("unknown"); got.MaxRetries != 5 {
		t.Fatalf("retryFor default should use global retry, got %+v", got)
	}
}

// TestNewFromConfigEventRetryOverridesGlobal 验证事件级 retry 覆盖全局配置。
func TestNewFromConfigEventRetryOverridesGlobal(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  retry:
    max_retries: 5
  events:
    hello:
      route:
        transport: kafka
      retry:
        max_retries: 1
`), map[string]Transport{"kafka": newFakeTransport()})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	broker := b.(*broker)
	if got := broker.retryFor("hello"); got.MaxRetries != 1 {
		t.Fatalf("event retry should override global, got %+v", got)
	}
}

// TestNewFromConfigUnknownTransport 验证未知 transport 标识报错。
func TestNewFromConfigUnknownTransport(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  events:
    hello:
      route:
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
  events:
    hello:
      route:
        transport: kafka
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "kafka"`) {
		t.Fatalf("expected unknown transport error for disabled kafka, got %v", err)
	}
}

// TestNewFromConfigNilEntrySkipped 验证字面 nil 条目被防御性跳过，
// 且路由引用 nil 条目按未知标识报错（resolve 表视 nil 为未提供）。
func TestNewFromConfigNilEntrySkipped(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": nil})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil broker")
	}
	// 路由引用 nil 条目：视为未提供，报未知标识错误。
	_, err = NewFromConfig(builderTestConfig(t, `
pubsub:
  events:
    hello:
      route:
        transport: kafka
`), map[string]Transport{"kafka": nil})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "kafka"`) {
		t.Fatalf("expected unknown transport error for nil entry, got %v", err)
	}
}

// TestNewFromConfigMemoryFallback 验证提供 memory 时未路由 topic 回退到它。
func TestNewFromConfigMemoryFallback(t *testing.T) {
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Publish(t.Context(), "anything", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish to memory fallback: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "anything" {
		t.Fatalf("published to %v, want [anything]", got)
	}
}

// TestNewFromConfigNoMemoryNoFallback 验证未提供 memory 时未路由 topic 发布报错。
func TestNewFromConfigNoMemoryNoFallback(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Publish(t.Context(), "unrouted", NewMessage([]byte("x"))); err == nil {
		t.Fatal("expected error publishing unrouted topic without default transport")
	}
}
