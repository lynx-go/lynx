package eventbus

import (
	"maps"
	"time"
)

// Wire 协议键：RawEvent ↔ 底层消息（Watermill metadata 等）的唯一映射点。
// Transport / Bus 实现必须经此编解码，禁止各自再写一份不一致逻辑。
const (
	MetaMessageKey   = "x-message-key"
	MetaEventTime    = "x-event-time"
	MetaLogicalTopic = "x-logical-topic"
)

// isProtocolMetaKey reports whether k is a wire protocol key (not a business header).
func isProtocolMetaKey(k string) bool {
	switch k {
	case MetaMessageKey, MetaEventTime, MetaLogicalTopic:
		return true
	default:
		return false
	}
}

// EncodeWireMetadata builds metadata from RawEvent.
// Business Headers are copied first (protocol keys in Headers are skipped),
// then protocol fields are set so they cannot be overwritten by Headers.
func EncodeWireMetadata(e *RawEvent) map[string]string {
	meta := map[string]string{}
	if e == nil {
		return meta
	}
	for k, v := range e.Headers {
		if isProtocolMetaKey(k) {
			continue
		}
		meta[k] = v
	}
	if e.Key != "" {
		meta[MetaMessageKey] = e.Key
	}
	if e.Topic != "" {
		meta[MetaLogicalTopic] = e.Topic
	}
	if !e.Time.IsZero() {
		meta[MetaEventTime] = e.Time.UTC().Format(time.RFC3339Nano)
	}
	return meta
}

// DecodeWireMetadata reconstructs RawEvent from wire id/payload/metadata.
// Protocol keys are restored onto Key/Topic/Time and stripped from Headers.
// Missing x-event-time falls back to time.Now() (see design O3).
func DecodeWireMetadata(id string, payload []byte, meta map[string]string) *RawEvent {
	e := &RawEvent{
		ID:      id,
		Payload: payload,
		Headers: map[string]string{},
		Time:    time.Now(),
	}
	if meta == nil {
		return e
	}
	if k := meta[MetaMessageKey]; k != "" {
		e.Key = k
	}
	if topic := meta[MetaLogicalTopic]; topic != "" {
		e.Topic = topic
	}
	if ts := meta[MetaEventTime]; ts != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Time = parsed
		}
	}
	maps.Copy(e.Headers, meta)
	delete(e.Headers, MetaMessageKey)
	delete(e.Headers, MetaEventTime)
	delete(e.Headers, MetaLogicalTopic)
	return e
}
