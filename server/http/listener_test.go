package http

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"
)

// TestWithListenerServesOnInjectedListener 验证 WithListener 注入 bufconn：
// Start 跳过 net.Listen，Addr()/Ready() 反映注入监听器，请求经监听器往返。
func TestWithListenerServesOnInjectedListener(t *testing.T) {
	ln := bufconn.Listen(64 * 1024)

	handler := http.NewServeMux()
	handler.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hi")
	})
	srv := NewServer(handler, WithListener(ln))

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

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return ln.DialContext(ctx)
			},
		},
	}
	resp, err := client.Get("http://bufconn/hello")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if string(body) != "hi" {
		t.Errorf("body = %q, want %q", string(body), "hi")
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
