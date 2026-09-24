package eventbus

import (
	"context"
	"strings"
	"testing"
)

// claimTransport 是最小 Transport 实现（占用检查测试用）。
type claimTransport struct {
	mode DeliveryMode
}

func (t *claimTransport) Publish(context.Context, string, *RawEvent) error { return nil }
func (t *claimTransport) Subscribe(context.Context, string, SubscribeOptions) (<-chan Delivery, error) {
	return nil, nil
}
func (t *claimTransport) Topics() []string           { return nil }
func (t *claimTransport) Close() error               { return nil }
func (t *claimTransport) DeliveryMode() DeliveryMode { return t.mode }

// groupingTransport 在最小实现上声明默认消费组（DefaultGrouper）。
type groupingTransport struct {
	claimTransport
	group string
}

func (t *groupingTransport) DefaultGroup(string) (string, bool) { return t.group, t.group != "" }

// nonComparableTransport 是值类型不可比较的 Transport（切片底层类型）：
// 占用检查必须放行而不是写入 map 时 panic。
type nonComparableTransport []string

func (nonComparableTransport) Publish(context.Context, string, *RawEvent) error { return nil }
func (nonComparableTransport) Subscribe(context.Context, string, SubscribeOptions) (<-chan Delivery, error) {
	return nil, nil
}
func (nonComparableTransport) Topics() []string           { return nil }
func (nonComparableTransport) Close() error               { return nil }
func (nonComparableTransport) DeliveryMode() DeliveryMode { return DeliveryConsumerGroup }

// TestEffectiveGroup：显式组优先；否则 DefaultGrouper；nil/非 Grouper 为空。
func TestEffectiveGroup(t *testing.T) {
	plain := &claimTransport{mode: DeliveryConsumerGroup}
	grouped := &groupingTransport{claimTransport: claimTransport{mode: DeliveryConsumerGroup}, group: "g1"}

	if got := EffectiveGroup(grouped, "k", "explicit"); got != "explicit" {
		t.Errorf("explicit group = %q, want explicit", got)
	}
	if got := EffectiveGroup(grouped, "k", ""); got != "g1" {
		t.Errorf("default group = %q, want g1", got)
	}
	if got := EffectiveGroup(plain, "k", ""); got != "" {
		t.Errorf("non-grouper = %q, want empty", got)
	}
	if got := EffectiveGroup(nil, "k", ""); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
}

// TestGroupClaims：ConsumerGroup 后端上同键第二 handler 被拒；同名幂等；
// Release 后可重登记；Broadcast 与不可比较 Transport 放行。
func TestGroupClaims(t *testing.T) {
	var c GroupClaims
	ct := &claimTransport{mode: DeliveryConsumerGroup}

	if err := c.Claim(ct, "key", "g", "h1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := c.Claim(ct, "key", "g", "h1"); err != nil {
		t.Fatalf("same handler re-claim must be idempotent: %v", err)
	}
	err := c.Claim(ct, "key", "g", "h2")
	if err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("second handler claim = %v, want already-consumed error", err)
	}
	// 不同组互不影响。
	if err := c.Claim(ct, "key", "g2", "h2"); err != nil {
		t.Fatalf("distinct group claim: %v", err)
	}
	// Release 后可重登记。
	c.Release(ct, "key", "g")
	if err := c.Claim(ct, "key", "g", "h3"); err != nil {
		t.Fatalf("claim after release: %v", err)
	}

	// Broadcast 后端不受占用约束。
	var cb GroupClaims
	bt := &claimTransport{mode: DeliveryBroadcast}
	if err := cb.Claim(bt, "key", "g", "h1"); err != nil {
		t.Fatalf("broadcast claim: %v", err)
	}
	if err := cb.Claim(bt, "key", "g", "h2"); err != nil {
		t.Fatalf("broadcast second claim: %v", err)
	}
	// nil Transport 放行。
	if err := cb.Claim(nil, "key", "g", "h1"); err != nil {
		t.Fatalf("nil transport claim: %v", err)
	}
	// 不可比较 Transport 放行（不 panic）。
	var cn GroupClaims
	if err := cn.Claim(nonComparableTransport{}, "key", "g", "h1"); err != nil {
		t.Fatalf("non-comparable claim: %v", err)
	}
	if err := cn.Claim(nonComparableTransport{}, "key", "g", "h2"); err != nil {
		t.Fatalf("non-comparable second claim: %v", err)
	}
	cn.Release(nonComparableTransport{}, "key", "g") // 不 panic
}

// TestGroupClaimsDefaultGroupCollision：显式组恰好等于另一 handler 留空的
// 默认组时同样拒绝（有效组键闭合原已知局限）。
func TestGroupClaimsDefaultGroupCollision(t *testing.T) {
	var c GroupClaims
	grouped := &groupingTransport{claimTransport: claimTransport{mode: DeliveryConsumerGroup}, group: "g1"}

	if err := c.Claim(grouped, "key", "", "h1"); err != nil {
		t.Fatalf("default-group claim: %v", err)
	}
	if err := c.Claim(grouped, "key", "g1", "h2"); err == nil {
		t.Fatal("explicit group equal to default group must be rejected")
	}
}
