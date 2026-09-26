package eventbus

import (
	"testing"
	"time"
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

// TestResolveSubscription 锁定唯一解析产物的完整优先级矩阵：
//   - HandlerName 空 → topic；
//   - MaxInFlight / AutoAck / ContinueOnError：调用/Topic 携带值优先，
//     空缺由 Options.Topics[t] 填充（bool 为并集，只可能被打开）；
//   - Retry / HandlerTimeout：调用值 > Topics[t] > 全局 > 默认；
//   - HandlerTimeout 负值（调用级或主题级）= 显式禁用（归一 0）；
//   - MaxInFlight 负值原样保留（不限制）；未知 topic 不改变任何默认。
func TestResolveSubscription(t *testing.T) {
	callRetry := &RetryOptions{MaxRetries: 1}
	cfgRetry := &RetryOptions{MaxRetries: 2}
	globalRetry := &RetryOptions{MaxRetries: 5}
	defaultRetry := RetryOptions{MaxRetries: 3}

	tests := []struct {
		name  string
		opts  Options
		topic string
		in    SubscribeOptions
		want  ResolvedSubscription
	}{
		{
			name:  "defaults with handler name fallback",
			topic: "t",
			want:  ResolvedSubscription{HandlerName: "t", Retry: defaultRetry},
		},
		{
			name:  "explicit handler name wins",
			topic: "t",
			in:    SubscribeOptions{HandlerName: "h"},
			want:  ResolvedSubscription{HandlerName: "h", Retry: defaultRetry},
		},
		{
			name: "topic config fills all five",
			opts: Options{Topics: map[string]TopicConfig{
				"t": {MaxInFlight: 4, HandlerTimeout: 2 * time.Second, AutoAck: true, ContinueOnError: true, Retry: cfgRetry},
			}},
			topic: "t",
			want: ResolvedSubscription{
				HandlerName: "t", MaxInFlight: 4, HandlerTimeout: 2 * time.Second,
				AutoAck: true, ContinueOnError: true, Retry: *cfgRetry,
			},
		},
		{
			name: "call-level wins over topic config",
			opts: Options{Topics: map[string]TopicConfig{
				"t": {MaxInFlight: 4, HandlerTimeout: 2 * time.Second, Retry: cfgRetry},
			}},
			topic: "t",
			in:    SubscribeOptions{MaxInFlight: 2, HandlerTimeout: 3 * time.Second, Retry: callRetry},
			want: ResolvedSubscription{
				HandlerName: "t", MaxInFlight: 2, HandlerTimeout: 3 * time.Second, Retry: *callRetry,
			},
		},
		{
			name:  "auto_ack and continue_on_error are union",
			opts:  Options{Topics: map[string]TopicConfig{"t": {AutoAck: true, ContinueOnError: true}}},
			topic: "t",
			in:    SubscribeOptions{AutoAck: false, ContinueOnError: false},
			want:  ResolvedSubscription{HandlerName: "t", AutoAck: true, ContinueOnError: true, Retry: defaultRetry},
		},
		{
			name:  "global retry and timeout fallback",
			opts:  Options{Retry: globalRetry, HandlerTimeout: 5 * time.Second},
			topic: "other",
			want:  ResolvedSubscription{HandlerName: "other", HandlerTimeout: 5 * time.Second, Retry: *globalRetry},
		},
		{
			name: "topic config beats global",
			opts: Options{
				Retry:          globalRetry,
				HandlerTimeout: 5 * time.Second,
				Topics:         map[string]TopicConfig{"t": {Retry: cfgRetry, HandlerTimeout: 2 * time.Second}},
			},
			topic: "t",
			want:  ResolvedSubscription{HandlerName: "t", HandlerTimeout: 2 * time.Second, Retry: *cfgRetry},
		},
		{
			name: "call-level negative handler timeout disables",
			opts: Options{
				HandlerTimeout: 5 * time.Second,
				Topics:         map[string]TopicConfig{"t": {HandlerTimeout: 2 * time.Second}},
			},
			topic: "t",
			in:    SubscribeOptions{HandlerTimeout: -1},
			want:  ResolvedSubscription{HandlerName: "t", Retry: defaultRetry},
		},
		{
			name: "topic-level negative handler timeout disables global",
			opts: Options{
				HandlerTimeout: 5 * time.Second,
				Topics:         map[string]TopicConfig{"t": {HandlerTimeout: -1}},
			},
			topic: "t",
			want:  ResolvedSubscription{HandlerName: "t", Retry: defaultRetry},
		},
		{
			name:  "negative max in flight preserved",
			opts:  Options{Topics: map[string]TopicConfig{"t": {MaxInFlight: 4}}},
			topic: "t",
			in:    SubscribeOptions{MaxInFlight: -1},
			want:  ResolvedSubscription{HandlerName: "t", MaxInFlight: -1, Retry: defaultRetry},
		},
		{
			name:  "unknown topic keeps defaults",
			opts:  Options{Topics: map[string]TopicConfig{"t": {MaxInFlight: 4, AutoAck: true, Retry: cfgRetry}}},
			topic: "missing",
			want:  ResolvedSubscription{HandlerName: "missing", Retry: defaultRetry},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(tt.opts)
			if got := r.ResolveSubscription(tt.topic, tt.in); got != tt.want {
				t.Errorf("ResolveSubscription(%q, %+v) = %+v, want %+v", tt.topic, tt.in, got, tt.want)
			}
		})
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
