package grpc

import (
	"context"
	"testing"
	"time"
)

// TestStopBeforeStartNormalizesErrServerStopped 回归 D4：Stop 先于 Start
// 执行时，底层 grpc.Server 已 GracefulStop，随后的 Serve 直接返回哨兵错误
// grpc.ErrServerStopped——须归一化为 nil（正常关停不是服务失败，不得发布
// lynx.service.failed 虚假事件）。修复前该错误原样上抛。
func TestStopBeforeStartNormalizesErrServerStopped(t *testing.T) {
	srv := NewServer(WithAddr("127.0.0.1:0"), WithRequestLog(false))

	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() = %v, want nil (grpc.ErrServerStopped must be normalized)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after prior Stop")
	}
}
