package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

// TestWithListenerServesOnInjectedListener 验证 WithListener 注入 bufconn：
// Start 跳过 net.Listen，Addr()/Ready() 反映注入监听器，客户端经监听器
// 完成 RPC（用框架默认注册的 gRPC 健康服务验证连通）。
func TestWithListenerServesOnInjectedListener(t *testing.T) {
	ln := bufconn.Listen(64 * 1024)

	srv := NewServer(WithListener(ln), WithRequestLog(false))

	startErr := make(chan error, 1)
	go func() {
		startErr <- srv.Start(context.Background())
	}()

	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server not ready within 3s")
	}
	if srv.Addr() == "" {
		t.Errorf("Addr() = empty, want the injected listener address")
	}

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	checkCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := grpc_health_v1.NewHealthClient(conn).Check(checkCtx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check() error = %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("health status = %v, want SERVING", resp.Status)
	}

	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil {
			t.Errorf("Start() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}
