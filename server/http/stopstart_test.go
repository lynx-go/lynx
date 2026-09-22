package http

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestStopBeforeStartAbortsServe 回归 D3：Stop 先于 Start 执行（模拟启动期
// 中断交错）时，Start 不得进入永久 Serve——应在 stopRequested 守卫处中止
// 并返回 nil。修复前：Stop 见 httpServer 为 nil 先返回，Start 随后 Listen
// 并永久阻塞在 Serve（挂死 + 监听 goroutine 泄漏）。
func TestStopBeforeStartAbortsServe(t *testing.T) {
	srv := NewServer(http.NewServeMux(), WithAddr("127.0.0.1:0"))

	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() = %v, want nil (interrupted start must be normalized)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after prior Stop (D3: leaked into permanent Serve)")
	}
}
