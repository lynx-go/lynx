package pubsub

import (
	"context"
	"testing"
	"time"
)

// TestRouterInitNilAppContext 回归 P1-5：脱离框架单用时 Init(nil) 不得
// panic——logger 与订阅 ctx 均取兜底值（slog.Default / context.Background）。
func TestRouterInitNilAppContext(t *testing.T) {
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	r := NewRouter(b, []Handler{
		NewTypedHandler("orders", "orderHandler", func(ctx context.Context, event *TypedMessage[string]) error {
			return nil
		}),
	})
	// 空 handler 列表同样必须容忍 nil（最精简的构造）。
	if err := NewRouter(b, nil).Init(nil); err != nil {
		t.Fatalf("Init(nil) with no handlers: %v", err)
	}
	if err := r.Init(nil); err != nil {
		t.Fatalf("Init(nil): %v", err)
	}
	if r.logger == nil {
		t.Fatal("logger must not be nil after Init(nil)")
	}
}

// TestRouterLifecycle 验证 Router 的正常生命周期：Start 阻塞至传入 ctx
// 取消，Stop 直接返回（无内部状态需要清理）。
func TestRouterLifecycle(t *testing.T) {
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	r := NewRouter(b, nil)
	if err := r.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	startCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(startCtx) }()

	select {
	case err := <-done:
		t.Fatalf("Start returned before ctx cancel: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}

// TestRouterStopBeforeStart 验证 Stop-before-Start 天然安全：Stop 无内部
// 状态可取消，直接返回 nil，不 panic、不挂死。
func TestRouterStopBeforeStart(t *testing.T) {
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	r := NewRouter(b, nil)
	_ = r.Stop(context.Background())
	_ = r.Init(newFakeApp())
	_ = r.Stop(context.Background())
}
