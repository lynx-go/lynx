package eventbus

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// noopTransport 是最小 Transport 实现：仅用于触发"内存 Bus 不读该字段"的 Warn。
type noopTransport struct{}

func (noopTransport) Publish(context.Context, string, *RawEvent) error { return nil }
func (noopTransport) Subscribe(context.Context, string, SubscribeOptions) (<-chan Delivery, error) {
	return nil, nil
}
func (noopTransport) Topics() []string { return nil }
func (noopTransport) Close() error     { return nil }

func newWarnBus(t *testing.T, opts Options) (*memoryBus, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	bus := NewMemoryBus(opts).(*memoryBus)
	if err := bus.Init(&fakeInitContext{logger: logger}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return bus, &buf
}

// TestMemoryBusWarnsInapplicableOptions：memory 不读的 watermill 选项在
// Init 期可见（不硬失败：同一份配置可能换后端复用）。
func TestMemoryBusWarnsInapplicableOptions(t *testing.T) {
	_, buf := newWarnBus(t, Options{
		Debug:            true,
		Transports:       []Transport{noopTransport{}},
		DefaultTransport: noopTransport{},
	})
	if !strings.Contains(buf.String(), "ignores Debug/Transports/DefaultTransport") {
		t.Fatalf("logs = %q, want inapplicable-options warning", buf.String())
	}
}

// TestMemoryBusWarnsMaxInFlightIgnored：内存 Bus 每 handler 串行处理，
// 在途上限的配置意图被忽略时记 Warn，不静默。
func TestMemoryBusWarnsMaxInFlightIgnored(t *testing.T) {
	bus, buf := newWarnBus(t, Options{})
	defer func() { _ = bus.Stop(context.Background()) }()
	if err := bus.Subscribe(context.Background(), "t", func(context.Context, *RawEvent) error { return nil }, withMaxInFlight(4)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if !strings.Contains(buf.String(), "ignores max_in_flight") {
		t.Fatalf("logs = %q, want max_in_flight-ignored warning", buf.String())
	}
}
