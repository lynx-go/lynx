package watermill

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/eventbus"
)

// subscriptionKey 是订阅复用单元：同一逻辑 topic 只建一条 transport 订阅，
// 多个 handler 挂载其上进程内并行扇出。逻辑 topic 参与键：不同逻辑 topic
// 即使路由到同一物理 key 也不合并（payload 类型与解码器不同）。
// 消费组 / 消费者成员数是后端配置（kafka consumer.*），不参与复用键。
type subscriptionKey = string

// topicSubscription 是一条 transport 订阅及其 handler 集合。
type topicSubscription struct {
	t           eventbus.Transport
	key         string // transport 侧键
	topic       string // 逻辑 topic
	maxInFlight int    // 订阅级在途上限（首个 handler 的解析值决定）

	mu       sync.Mutex
	handlers []*subscriptionHandler
}

// subscriptionHandler 是挂载在订阅上的单个 handler：投递语义 per-handler
// （Retry / HandlerTimeout / AutoAck / ContinueOnError），确认裁决按订阅聚合。
type subscriptionHandler struct {
	name     string
	fn       eventbus.HandlerFunc
	resolved eventbus.ResolvedSubscription
}

func newTopicSubscription(t eventbus.Transport, key, topic string, maxInFlight int) *topicSubscription {
	return &topicSubscription{t: t, key: key, topic: topic, maxInFlight: maxInFlight}
}

func (s *topicSubscription) add(h *subscriptionHandler) {
	s.mu.Lock()
	s.handlers = append(s.handlers, h)
	s.mu.Unlock()
}

// snapshot 返回 handler 集合快照（挂载在途时从下一条消息生效）。
func (s *topicSubscription) snapshot() []*subscriptionHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*subscriptionHandler(nil), s.handlers...)
}

// routerName 是订阅在 watermill router 内的唯一名。
func (s *topicSubscription) routerName() string {
	return "sub:" + s.topic
}

// attachHandler 把 handler 挂到其事件订阅上：订阅不存在则创建（首个
// handler 先入集合、注册成功后才入表）并注册订阅级 dispatcher，随后挂载
// handler。
func (b *Bus) attachHandler(topic, handlerName string, h eventbus.HandlerFunc, resolved eventbus.ResolvedSubscription) error {
	t, key, err := b.resolve(topic)
	if err != nil {
		return err
	}
	sk := subscriptionKey(topic)

	b.mu.Lock()
	sub, exists := b.subs[sk]
	if !exists {
		sub = newTopicSubscription(t, key, topic, resolved.MaxInFlight)
		// 首个 handler 必须先入集合：router 启动后不得以空 handler 集合
		// 消费（否则消息会被无 handler 地 Ack 丢失）。
		sub.add(&subscriptionHandler{name: handlerName, fn: h, resolved: resolved})
		// 注册成功才入表：注册失败（panic 已在 addSubscription 内翻译为
		// 错误）不留半成品，并发订阅也不会挂到未注册的孤儿订阅上。
		if err := b.addSubscription(sub); err != nil {
			b.mu.Unlock()
			return err
		}
		b.subs[sk] = sub
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	// 订阅级 MaxInFlight 是事件级配置：首个 handler 的解析值生效；同名
	// 事件的两个 Topic 携带了不同值（配置错误）时后续值被忽略并 Warn。
	if resolved.MaxInFlight != sub.maxInFlight {
		b.logger.Warn("watermill: subscription max_in_flight mismatch ignored",
			"topic", topic, "max_in_flight", resolved.MaxInFlight, "effective", sub.maxInFlight)
	}
	sub.add(&subscriptionHandler{name: handlerName, fn: h, resolved: resolved})
	return nil
}

// DefaultMaxInFlight 是 MaxInFlight 未设置（0）时的订阅级在途上限默认值：
// 1 = 串行处理——有界（goroutine 上界 ≈ H+2/消息）且恢复同订阅处理顺序。
const DefaultMaxInFlight = 1

// addSubscription 在 router 内注册订阅级 dispatcher。panic 安全（WK-03）：
// watermill router 对重名 handler 直接 panic（DuplicateHandlerNameError），
// 正常路径已由订阅注册表拦截，但历史缺陷或外部操作残留的"幽灵订阅"会
// 绕过查重——动态订阅不得击穿进程，此处把 panic 翻译为错误返回；非
// duplicate panic 附 debug.Stack()（复审-8），未知 panic 源可排查。
func (b *Bus) addSubscription(sub *topicSubscription) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if dup, ok := r.(message.DuplicateHandlerNameError); ok {
				err = fmt.Errorf("%w %q: %s", errHandlerNameTaken, dup.HandlerName, dup.Error())
				return
			}
			err = fmt.Errorf("watermill: add subscription %q panicked: %v\n%s", sub.routerName(), r, debug.Stack())
		}
	}()
	// 订阅级在途上限：0 = 默认 1（串行）；负数 = 不限制（逃生口）。
	maxInFlight := sub.maxInFlight
	if maxInFlight == 0 {
		maxInFlight = DefaultMaxInFlight
	}
	var sem chan struct{}
	if maxInFlight > 0 {
		sem = make(chan struct{}, maxInFlight)
	}
	adapter := &subscriberAdapter{
		t: sub.t,
		// 后端特有字段（消费组 / 成员数）由 Transport 自己的配置决定，
		// Bus 不填；opts 参数保留为后端扩展缝。
		opts:       eventbus.SubscribeOptions{},
		forwardAck: b.forwardDeliveryAck,
		sem:        sem,
	}
	b.router.AddConsumerHandler(sub.routerName(), sub.key, adapter, func(msg *message.Message) error {
		return b.dispatch(sub, msg)
	})
	return nil
}

// dispatch 是订阅级投递执行器：并行调用全部 handler（各自 InvokeHandler：
// ctx 传播属性 / received 日志 / 固定退避重试），聚合确认。共享 offset 下
// 的失败语义（设计文档 design-eventbus-consumption.md §2.3）：
//   - 全部"参与裁决"的 handler 成功（或已止损跳过）→ nil（Router Ack）；
//   - 任一 handler 终态失败且未达重投上限 → 返回错误（Router Nack，整条
//     重投，所有参与裁决的 handler 重跑——业务必须幂等）；
//   - 某 handler 累计终态失败超过 max_redeliveries → 该 handler 自本轮起
//     被跳过并记 Error，不连坐其他 handler；全部成功 / 跳过后 Ack。
//
// AutoAck / ContinueOnError 的 handler 经 InvokeHandler 恒返回 nil，天然
// 不参与裁决。
func (b *Bus) dispatch(sub *topicSubscription, msg *message.Message) error {
	handlers := sub.snapshot()
	raw := FromMessage(msg)
	raw.Topic = sub.topic
	ctx := msg.Context()
	limit, hasLimit := b.maxRedeliveriesFor(sub.topic)

	results := make([]error, len(handlers))
	skipped := make([]bool, len(handlers))
	var wg sync.WaitGroup
	for i, h := range handlers {
		if hasLimit && b.redeliver.Count(h.name, raw.ID) > limit {
			// 已止损（超限当轮已记 Error）：跳过，不再连坐其他 handler。
			skipped[i] = true
			b.logger.DebugContext(ctx, "skipping handler exhausted by redelivery limit",
				"topic", sub.topic, "handler", h.name, "message_id", raw.ID)
			continue
		}
		wg.Add(1)
		go func(i int, h *subscriptionHandler) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					// Router 的 Recoverer 覆盖不到本 goroutine：panic 按该
					// handler 终态失败处理（对齐原 Recoverer → error → Nack）。
					results[i] = fmt.Errorf("watermill: handler %q panicked: %v\n%s", h.name, r, debug.Stack())
				}
			}()
			results[i] = b.invokeHandler(ctx, sub.topic, h, raw)
		}(i, h)
	}
	wg.Wait()

	var firstErr error
	for i, err := range results {
		if skipped[i] {
			continue
		}
		if err == nil {
			// 本 handler 成功：立即清自身计数（对齐原 per-handler 中间件
			// 语义）——否则跨轮重投时，交替失败/成功的 handler 会把累计
			// 失败推过上限，被误判为毒消息。
			b.redeliver.Success(handlers[i].name, raw.ID)
			continue
		}
		if hasLimit {
			if n := b.redeliver.Failure(handlers[i].name, raw.ID); n > limit {
				b.logger.ErrorContext(ctx, "handler exceeded max redeliveries, dropping for this handler",
					"topic", sub.topic, "handler", handlers[i].name,
					"key", raw.Key, "message_id", raw.ID,
					"redeliveries", n-1, "max_redeliveries", limit)
				continue // 该 handler 已止损，不触发整条重投
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr // Router → Nack → 整条重投
	}
	// 全成功 / 已止损：清除本消息全部 handler 的计数（含被跳过者），避免
	// 同 ID 的新投递被陈旧计数误伤。
	for _, h := range handlers {
		b.redeliver.Success(h.name, raw.ID)
	}
	return nil
}

// invokeHandler 执行单个 handler 的一次投递：重试 / 传播属性 / 日志由
// eventbus.InvokeHandler 承担，确认时序由订阅级 dispatcher 聚合。每个
// handler 拿到独立的 RawEvent 副本，避免 raw handler 互相污染。
func (b *Bus) invokeHandler(ctx context.Context, topic string, h *subscriptionHandler, raw *eventbus.RawEvent) error {
	return eventbus.InvokeHandler(ctx, b.logger, h.fn, eventbus.CloneRawEvent(raw), b.resolver, eventbus.InvokeOptions{
		Topic:       topic,
		HandlerName: h.name,
		Retry:       h.resolved.Retry,
		Once:        h.resolved.AutoAck,
		Swallow:     h.resolved.ContinueOnError,
		Timeout:     h.resolved.HandlerTimeout,
	})
}
