package eventbus

import (
	"testing"
)

// markerMarshaler 是可在断言中按值区分的序列化器。
type markerMarshaler string

func (m markerMarshaler) Marshal(any) ([]byte, error) { return []byte(m), nil }
func (m markerMarshaler) Unmarshal([]byte, any) error { return nil }

// TestResolverMarshalerPriority 锁定四级查找序：
// TopicMarshalers[t] → Topics[t].Marshaler → 全局 → JSON。
func TestResolverMarshalerPriority(t *testing.T) {
	global := markerMarshaler("global")
	topicCfg := markerMarshaler("topic-cfg")
	override := markerMarshaler("override")

	full := NewResolver(Options{
		Marshaler:       global,
		TopicMarshalers: map[string]Marshaler{"t": override},
		Topics:          map[string]TopicConfig{"t": {Marshaler: topicCfg}},
	})
	if got := full.MarshalerFor("t"); got != override {
		t.Errorf("MarshalerFor(t) = %v, want TopicMarshalers override", got)
	}

	cfgOnly := NewResolver(Options{
		Marshaler: global,
		Topics:    map[string]TopicConfig{"t": {Marshaler: topicCfg}},
	})
	if got := cfgOnly.MarshalerFor("t"); got != topicCfg {
		t.Errorf("MarshalerFor(t) = %v, want Topics[t].Marshaler", got)
	}

	globalOnly := NewResolver(Options{Marshaler: global})
	if got := globalOnly.MarshalerFor("x"); got != global {
		t.Errorf("MarshalerFor(x) = %v, want global marshaler", got)
	}

	if _, ok := NewResolver(Options{}).MarshalerFor("x").(JSONMarshaler); !ok {
		t.Errorf("MarshalerFor(x) = %T, want JSONMarshaler fallback", NewResolver(Options{}).MarshalerFor("x"))
	}
}

// TestResolverRetryPriority 锁定四级合并（高→低）：
// 调用级 call > Topics[t].Retry > 全局 > 默认 3 次。
func TestResolverRetryPriority(t *testing.T) {
	r := NewResolver(Options{
		Retry:  &RetryOptions{MaxRetries: 5},
		Topics: map[string]TopicConfig{"t": {Retry: &RetryOptions{MaxRetries: 2}}},
	})

	call := RetryOptions{MaxRetries: 1}
	if got := r.RetryFor("t", &call); got.MaxRetries != 1 {
		t.Errorf("call-level = %d, want 1", got.MaxRetries)
	}
	if got := r.RetryFor("t", nil); got.MaxRetries != 2 {
		t.Errorf("topic config = %d, want 2", got.MaxRetries)
	}
	if got := r.RetryFor("other", nil); got.MaxRetries != 5 {
		t.Errorf("global = %d, want 5", got.MaxRetries)
	}
	if got := NewResolver(Options{}).RetryFor("x", nil); got.MaxRetries != 3 {
		t.Errorf("default = %d, want 3", got.MaxRetries)
	}
}

// TestResolverApplyTopicDefaults 锁定 Topic 默认值的填充语义：
// 显式调用选项优先（只填空缺），未知 topic 不改变选项。
func TestResolverApplyTopicDefaults(t *testing.T) {
	r := NewResolver(Options{Topics: map[string]TopicConfig{
		"t": {MaxInFlight: 4, AutoAck: true, ContinueOnError: true},
	}})

	o := &SubscribeOptions{MaxInFlight: 2}
	r.ApplyTopicDefaults("t", o)
	if o.MaxInFlight != 2 {
		t.Errorf("MaxInFlight = %d, want explicit to win", o.MaxInFlight)
	}
	if !o.AutoAck || !o.ContinueOnError {
		t.Errorf("defaults not filled: %+v", *o)
	}

	untouched := &SubscribeOptions{}
	r.ApplyTopicDefaults("missing", untouched)
	if *untouched != (SubscribeOptions{}) {
		t.Errorf("unknown topic changed options: %+v", *untouched)
	}
}

// TestResolverLogAndPropagateDefaults 锁定日志/传播键默认值。
func TestResolverLogAndPropagateDefaults(t *testing.T) {
	r := NewResolver(Options{})
	if got := r.LogMessageFor("t"); got != (LogMessageOptions{}) {
		t.Errorf("LogMessageFor = %+v, want zero", got)
	}
	keys := r.PropagateKeys()
	if len(keys) != 2 || keys[0] != "request_id" || keys[1] != "user_id" {
		t.Errorf("PropagateKeys = %v, want [request_id user_id]", keys)
	}
	if got := NewResolver(Options{PropagateAttrs: []string{}}).PropagateKeys(); len(got) != 0 {
		t.Errorf("PropagateKeys(empty) = %v, want disabled", got)
	}
}
