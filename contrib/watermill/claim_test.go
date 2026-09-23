package watermill_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

// modeFakeTransport 是可配置投递模式与默认组的假 Transport：订阅/发布
// 行为无关紧要，测试只驱动 Bus 的消费组占用检查（WK-01）。
type modeFakeTransport struct {
	mode   eventbus.DeliveryMode
	defGrc string // DefaultGrouper 返回值；空串表示未实现该能力（nil 接口不成立时）
	hasDef bool   // 是否实现 DefaultGrouper
}

func (f *modeFakeTransport) Publish(ctx context.Context, topic string, e *eventbus.RawEvent) error {
	return nil
}
func (f *modeFakeTransport) Subscribe(ctx context.Context, topic string, opts eventbus.SubscribeOptions) (<-chan eventbus.Delivery, error) {
	return nil, nil
}
func (f *modeFakeTransport) Topics() []string { return nil }
func (f *modeFakeTransport) Close() error     { return nil }
func (f *modeFakeTransport) DeliveryMode() eventbus.DeliveryMode {
	return f.mode
}
func (f *modeFakeTransport) DefaultGroup(key string) (string, bool) {
	if !f.hasDef {
		return "", false
	}
	return f.defGrc, true
}

// TestGroupClaimRejectedOnConsumerGroupTransport 钉住占用检查经
// DeliveryMode 声明启用（原"非内存 Transport 即检查"的身份推断已由
// 契约取代）：ConsumerGroup 后端上两个 handler 共用同 topic+组被拒绝。
func TestGroupClaimRejectedOnConsumerGroupTransport(t *testing.T) {
	bus := watermill.New(eventbus.Options{
		DefaultTransport: &modeFakeTransport{mode: eventbus.DeliveryConsumerGroup},
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	h := func(context.Context, *eventbus.RawEvent) error { return nil }
	if err := bus.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h1"), eventbus.WithGroup("svc")); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	err := bus.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h2"), eventbus.WithGroup("svc"))
	if err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("want WK-01 rejection for shared group, got: %v", err)
	}
}

// TestGroupClaimSkippedOnBroadcastTransport：广播后端（进程内语义）不受
// 组占用约束——两个 handler 共用同组名是合法的多订阅者共存。
func TestGroupClaimSkippedOnBroadcastTransport(t *testing.T) {
	bus := watermill.New(eventbus.Options{
		DefaultTransport: &modeFakeTransport{mode: eventbus.DeliveryBroadcast},
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	h := func(context.Context, *eventbus.RawEvent) error { return nil }
	if err := bus.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h1"), eventbus.WithGroup("svc")); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	if err := bus.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h2"), eventbus.WithGroup("svc")); err != nil {
		t.Fatalf("broadcast transport must allow multiple subscribers: %v", err)
	}
}

// TestGroupClaimDefaultGroupBlindSpotClosed 回归 claimGroup 原已知局限一：
// handler A 留空组（实际走后端默认组 "svc"，经 DefaultGrouper 声明）、
// handler B 显式 WithGroup("svc")——有效组相同，必须拒绝（此前 claim 键
// 的组名不同，静默放行导致分区瓜分）。
func TestGroupClaimDefaultGroupBlindSpotClosed(t *testing.T) {
	bus := watermill.New(eventbus.Options{
		DefaultTransport: &modeFakeTransport{
			mode:   eventbus.DeliveryConsumerGroup,
			defGrc: "svc",
			hasDef: true,
		},
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	h := func(context.Context, *eventbus.RawEvent) error { return nil }
	// A：留空组 → 有效组为默认组 svc。
	if err := bus.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h1")); err != nil {
		t.Fatalf("first Subscribe (empty group): %v", err)
	}
	// B：显式 svc → 与 A 的有效组相同，必须拒绝。
	err := bus.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h2"), eventbus.WithGroup("svc"))
	if err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("want rejection for explicit group == default group, got: %v", err)
	}
	// 反向：A 显式、B 留空，同样必须拒绝。
	bus2 := watermill.New(eventbus.Options{
		DefaultTransport: &modeFakeTransport{
			mode:   eventbus.DeliveryConsumerGroup,
			defGrc: "svc",
			hasDef: true,
		},
	})
	if err := bus2.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bus2.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h1"), eventbus.WithGroup("svc")); err != nil {
		t.Fatalf("first Subscribe (explicit group): %v", err)
	}
	err = bus2.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h2"))
	if err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("want rejection for empty group falling back to taken default, got: %v", err)
	}
	// 不同组（广播语义）仍然放行。
	if err := bus2.Subscribe(context.Background(), "orders", h, eventbus.WithHandlerName("h3"), eventbus.WithGroup("other")); err != nil {
		t.Fatalf("distinct group must be allowed: %v", err)
	}
}
