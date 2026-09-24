package eventbus

import (
	"fmt"
	"reflect"
	"sync"
)

// EffectiveGroup 计算订阅键上的有效消费组：显式 group 优先；为空时取
// ConsumerGroup 后端声明的配置默认组（DefaultGrouper，如 kafka
// consumer.group_id）；nil 或非 Grouper 返回空。
func EffectiveGroup(t Transport, key, group string) string {
	if group != "" {
		return group
	}
	if t == nil {
		return ""
	}
	if dg, ok := t.(DefaultGrouper); ok {
		if g, ok := dg.DefaultGroup(key); ok {
			return g
		}
	}
	return ""
}

// claimKey 是 GroupClaims 的键：transport × 订阅键 × 有效组。
type claimKey struct {
	t     Transport
	key   string
	group string
}

// GroupClaims 跟踪 ConsumerGroup 类型 Transport 上（订阅键 × 有效消费组）
// 的 handler 占用：同一键上的第二个 handler 必须被拒绝——否则分区被静默
// 瓜分、各收一半消息。零值可用，并发安全。
//
// 约定：Bus 无 Unsubscribe API 时占用不释放（宁可误拒，不可静默瓜分）；
// 订阅登记失败路径必须调用 Release 回滚，否则残留占用永久锁死该 topic+组。
// 已知局限（claim 粒度）：两个不同逻辑 topic 路由到同一 Transport、物理
// topics 重叠、又共用同一消费组时，claim 键互不相同，本检查不拦截——此类
// 部署必须为各逻辑 topic 显式配置互不相同的消费组。
type GroupClaims struct {
	mu     sync.Mutex
	claims map[claimKey]string
}

// Claim 登记占用；已被其他 handler 占用时返回明确错误。Broadcast 后端与
// 不可比较的 Transport 值类型直接放行（后者无法作为 map 键跟踪，放弃检查
// 而非在 map 写入时 panic；仓库内实现均为指针型）。同名 handler 重复登记
// 是幂等的。
func (c *GroupClaims) Claim(t Transport, key, group, handler string) error {
	if !claimable(t) {
		return nil
	}
	eff := EffectiveGroup(t, key, group)
	k := claimKey{t: t, key: key, group: eff}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claims == nil {
		c.claims = map[claimKey]string{}
	}
	if prev, ok := c.claims[k]; ok && prev != handler {
		groupLabel := eff
		if groupLabel == "" {
			groupLabel = "(no group)"
		}
		return fmt.Errorf(
			"transport key %q (group %s) is already consumed by handler %q on %T; "+
				"two handlers sharing one consumer group silently split partitions and each receives only part of the messages",
			key, groupLabel, prev, t)
	}
	c.claims[k] = handler
	return nil
}

// Release 回滚占用（订阅登记失败路径）。与 Claim 的跳过条件保持一致：
// 不可比较的 Transport 键连 delete 都会 panic，绝不能直接 delete。
func (c *GroupClaims) Release(t Transport, key, group string) {
	if !claimable(t) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.claims, claimKey{t: t, key: key, group: EffectiveGroup(t, key, group)})
}

// claimable 报告该 Transport 是否需要且能够进行占用检查：ConsumerGroup
// 后端需要；Broadcast 后端与不可比较的值类型不需要/无法跟踪。
func claimable(t Transport) bool {
	if t == nil || t.DeliveryMode() != DeliveryConsumerGroup {
		return false
	}
	return reflect.TypeOf(t).Comparable()
}
