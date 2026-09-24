package lynx

import (
	"context"
	"testing"
	"time"
)

// nonBlockingService 的 Start 只做非阻塞动作，靠 WaitForShutdown 保持
// actor 存活——服务作者最常见的非阻塞形态。
type nonBlockingService struct {
	started chan struct{}
}

func (s *nonBlockingService) Name() string               { return "non-blocking" }
func (s *nonBlockingService) Init(AppContext) error      { return nil }
func (s *nonBlockingService) Stop(context.Context) error { return nil }

func (s *nonBlockingService) Start(ctx context.Context) error {
	close(s.started)
	return WaitForShutdown(ctx)
}

// TestWaitForShutdownBlocksUntilCancel：取消前不返回，取消后返回 nil。
func TestWaitForShutdownBlocksUntilCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- WaitForShutdown(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("WaitForShutdown() returned before cancellation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForShutdown() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForShutdown() did not return after cancellation")
	}
}

// TestWaitForShutdownReturnsNilOnCanceledContext：已取消的 ctx 立即返回
// nil（而非 ctx.Err()）——Start 关停时正常退出，不产生关停错误。
func TestWaitForShutdownReturnsNilOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := WaitForShutdown(ctx); err != nil {
		t.Fatalf("WaitForShutdown() on canceled ctx error = %v, want nil", err)
	}
}

// TestWaitForShutdownKeepsNonBlockingServiceAlive 是消费方接缝测试：
// Start 只做非阻塞动作的服务不会让应用提前退出；Close 后 Run 返回 nil。
func TestWaitForShutdownKeepsNonBlockingServiceAlive(t *testing.T) {
	app, err := newLynx(NewOptions(WithIsolated()))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	svc := &nonBlockingService{started: make(chan struct{})}
	app.Register(svc)

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()

	select {
	case <-svc.started:
	case <-time.After(5 * time.Second):
		t.Fatal("service Start was not invoked")
	}

	// 应用仍在运行：Start 直接返回会立即触发关停，WaitForShutdown 保持存活。
	select {
	case err := <-runErr:
		t.Fatalf("Run() returned early (service did not keep app alive): %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	app.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Close()")
	}
}
