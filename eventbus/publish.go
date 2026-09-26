package eventbus

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx/internal/propagation"
	"github.com/lynx-go/lynx/logging"
)

// CloneRawEvent 深拷贝 RawEvent：Headers 一律克隆为非 nil，Payload 复制。
// Bus / Transport 实现转发前隔离副本使用（内存 dispatch 逐订阅者克隆、
// Watermill Bus 发布前克隆）。
func CloneRawEvent(e *RawEvent) *RawEvent {
	cp := *e
	cp.Headers = cloneHeaders(e.Headers)
	if e.Payload != nil {
		cp.Payload = append([]byte(nil), e.Payload...)
	}
	return &cp
}

func cloneHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	cp := make(map[string]string, len(h))
	maps.Copy(cp, h)
	return cp
}

// BuildRawEvent 是发布侧 RawEvent 组装的唯一归属（内存 Bus 与 Watermill Bus
// 共用，是设计文档 §5.1「单一映射点」在 Bus 层的延伸）：
// payload 类型分派（*RawEvent 透传 / []byte / nil / 类型化经 Marshaler）、
// 协议键清除、Metadata 合并、日志属性白名单传播、ID/Time 默认。
// 组装结果不再被共享修改：Headers 一律克隆、非 nil；Payload 切片由调用方
// 按需克隆（内存 dispatch 与 Watermill Transport 发布前各有克隆）。
func BuildRawEvent(ctx context.Context, b Bus, topic string, payload any, o *PublishOptions, propagateKeys []string) (*RawEvent, error) {
	var data []byte
	var headers map[string]string
	var key string
	id := uuid.NewString()
	var eventTime time.Time

	switch v := payload.(type) {
	case *RawEvent:
		if v == nil {
			return nil, fmt.Errorf("eventbus: payload is typed nil *RawEvent")
		}
		// 透传：保留 ID/Key/Headers/Time；逻辑 topic 以函数参数为准（与 Transport 路径一致）
		if v.ID != "" {
			id = v.ID
		}
		key = v.Key
		if o.MessageKey != "" {
			key = o.MessageKey
		}
		headers = cloneHeaders(v.Headers)
		data = v.Payload
		eventTime = v.Time
	case []byte:
		data = v
		key = o.MessageKey
		headers = map[string]string{}
	case nil:
		key = o.MessageKey
		headers = map[string]string{}
	default:
		m := ResolveMarshaler(b, topic, nil, o.Marshaler)
		bs, err := m.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("eventbus: marshal %q: %w", topic, err)
		}
		data = bs
		key = o.MessageKey
		headers = map[string]string{}
	}
	if headers == nil {
		headers = map[string]string{}
	}
	// 先合并业务 Metadata，再清除协议键，避免覆盖 x-message-key 等
	maps.Copy(headers, o.Metadata)
	for k := range headers {
		if isProtocolMetaKey(k) {
			delete(headers, k)
		}
	}
	// 传播日志属性（白名单），已存在的不覆盖；request_id/user_id 的值
	// 过共享校验（internal/propagation），非法值不进入消息头（下游会直接
	// 还原进日志）。
	for _, k := range propagateKeys {
		if _, ok := headers[k]; ok {
			continue
		}
		for _, a := range logging.AttrsFrom(ctx) {
			if a.Key != k {
				continue
			}
			v := a.Value.String()
			if isPropagationField(k) && !propagation.Valid(v) {
				continue
			}
			headers[k] = v
			break
		}
	}
	// 传播 trace 上下文（W3C traceparent / tracestate）：当前有 active span
	// 时注入，跨进程消费侧据此续链（全局 no-op propagator 下不写入）。
	injectTrace(ctx, headers)
	if eventTime.IsZero() {
		eventTime = time.Now()
	}
	return &RawEvent{
		ID:      id,
		Topic:   topic,
		Key:     key,
		Headers: headers,
		Payload: data,
		Time:    eventTime,
	}, nil
}
