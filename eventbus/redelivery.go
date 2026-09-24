package eventbus

import "sync"

// RedeliveryLimiter 按（handler × 消息 ID）计数终态失败轮数，用于阻断
// at-least-once 后端的无限重投（毒消息止损）。有界：容量满后按插入序
// 环形淘汰最老条目——计数是止损机制而非精确语义，被淘汰的陈旧键重新
// 计数是可接受的误差，换取消耗与流量无关的常数内存。须经
// NewRedeliveryLimiter 构造。
type RedeliveryLimiter struct {
	mu     sync.Mutex
	counts map[string]int
	order  []string // 环形槽位：新键顶掉最老槽位
	pos    int
}

// NewRedeliveryLimiter 创建容量为 capacity 的限制器（<=0 时取 4096）。
func NewRedeliveryLimiter(capacity int) *RedeliveryLimiter {
	if capacity <= 0 {
		capacity = 4096
	}
	return &RedeliveryLimiter{
		counts: make(map[string]int, capacity),
		order:  make([]string, capacity),
	}
}

// Failure 记录该 handler 对该消息的一轮终态失败，返回累计轮数（含本次）。
// 空消息 ID 无法跨轮追踪，返回大值让调用方立即止损。
func (l *RedeliveryLimiter) Failure(handlerName, id string) int {
	if id == "" {
		return 1 << 30
	}
	key := limiterKey(handlerName, id)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.counts[key]; !ok {
		if victim := l.order[l.pos]; victim != "" {
			delete(l.counts, victim)
		}
		l.order[l.pos] = key
		l.pos = (l.pos + 1) % len(l.order)
	}
	l.counts[key]++
	return l.counts[key]
}

// Count 返回 (handler, id) 已累计的终态失败轮数（未记录为 0）。供订阅级
// 投递器在调用前判断该 handler 是否已止损（超过上限即跳过）。
func (l *RedeliveryLimiter) Count(handlerName, id string) int {
	if id == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[limiterKey(handlerName, id)]
}

// Success 清除该 handler 对该消息的计数：消息处理成功即生命周期结束，
// 腾出容量给活跃键，也避免同 ID 的陈旧计数误伤后续投递（如上游按 ID
// 重发的新消息）。键含 handlerName，只清自身，不动其他 handler 对同一
// 消息的计数。
func (l *RedeliveryLimiter) Success(handlerName, id string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.counts, limiterKey(handlerName, id))
}

// limiterKey 拼接计数键：handlerName + "|" + 消息 ID。同一消息会投给同
// 订阅的多个 handler，纯消息 ID 的共享键会让成功侧清零失败侧的累计计数，
// 毒消息永不达上限。
func limiterKey(handlerName, id string) string {
	return handlerName + "|" + id
}
