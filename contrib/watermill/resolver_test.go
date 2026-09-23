package watermill_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

// markerMarshaler 是可在断言中按值区分的序列化器。
type markerMarshaler string

func (m markerMarshaler) Marshal(any) ([]byte, error) { return []byte(m), nil }
func (m markerMarshaler) Unmarshal([]byte, any) error { return nil }

// TestBusMarshalerForPriority 锁定 watermill 侧的四级查找序（此前该链
// 无任何测试，与 memory 逐字复制）：TopicMarshalers[t] →
// Topics[t].Marshaler → 全局 → JSON。
func TestBusMarshalerForPriority(t *testing.T) {
	global := markerMarshaler("global")
	topicCfg := markerMarshaler("topic-cfg")
	override := markerMarshaler("override")

	full := watermill.New(eventbus.Options{
		Marshaler:       global,
		TopicMarshalers: map[string]eventbus.Marshaler{"t": override},
		Topics:          map[string]eventbus.TopicConfig{"t": {Marshaler: topicCfg}},
	})
	if got := full.MarshalerFor("t"); got != override {
		t.Errorf("MarshalerFor(t) = %v, want TopicMarshalers override", got)
	}

	cfgOnly := watermill.New(eventbus.Options{
		Marshaler: global,
		Topics:    map[string]eventbus.TopicConfig{"t": {Marshaler: topicCfg}},
	})
	if got := cfgOnly.MarshalerFor("t"); got != topicCfg {
		t.Errorf("MarshalerFor(t) = %v, want Topics[t].Marshaler", got)
	}

	if got := watermill.New(eventbus.Options{Marshaler: global}).MarshalerFor("x"); got != global {
		t.Errorf("MarshalerFor(x) = %v, want global marshaler", got)
	}
	if _, ok := watermill.New(eventbus.Options{}).MarshalerFor("x").(eventbus.JSONMarshaler); !ok {
		t.Errorf("MarshalerFor(x) = %T, want JSONMarshaler fallback", watermill.New(eventbus.Options{}).MarshalerFor("x"))
	}
}

// TestWatermillTopicConfigRetryApplies：Options.Topics[t].Retry 经共享
// Resolver 进入投递执行器（集成钉：解析不再由每个 Bus 各自记得调用）。
func TestWatermillTopicConfigRetryApplies(t *testing.T) {
	bus := watermill.New(eventbus.Options{
		DefaultTransport: watermill.NewMemoryTransport(),
		Topics: map[string]eventbus.TopicConfig{
			"cfg.topic": {Retry: &eventbus.RetryOptions{MaxRetries: 2, Backoff: time.Millisecond}},
		},
	})
	startParityBus(t, bus)

	var calls atomic.Int32
	done := make(chan struct{})
	if err := bus.Subscribe(context.Background(), "cfg.topic", func(context.Context, *eventbus.RawEvent) error {
		if n := calls.Add(1); n == 3 {
			close(done)
			return nil
		}
		return errors.New("fail")
	}, eventbus.WithHandlerName("cfg-retry")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Publish(context.Background(), "cfg.topic", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("handler calls = %d, want 3 (topic-config retry must apply)", calls.Load())
	}
}

// loggerInitContext 以捕获 logger 初始化 Bus（Init 仅取 Logger）。
type loggerInitContext struct{ logger *slog.Logger }

func (c loggerInitContext) Context() context.Context        { return context.Background() }
func (c loggerInitContext) Logger(args ...any) *slog.Logger { return c.logger }

// TestWatermillWarnsBufferSize：memory 专属的 BufferSize 在 watermill 侧
// 被忽略时记 Warn（EnsureDefaults 之前判定，避免默认 64 误报）。
func TestWatermillWarnsBufferSize(t *testing.T) {
	var buf bytes.Buffer
	bus := watermill.New(eventbus.Options{BufferSize: 32})
	if err := bus.Init(loggerInitContext{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = bus.Stop(context.Background()) })
	if !strings.Contains(buf.String(), "BufferSize") {
		t.Fatalf("logs = %q, want BufferSize warning", buf.String())
	}
}
