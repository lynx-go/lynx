package main

import (
	"io"
	"net/http"
	"testing"

	"github.com/lynx-go/lynx/lynxtest"
)

// TestHelloEndpoint 是 L2 组装测试：走与生产 main 完全相同的 Setup，
// 只把环境换成测试版——地址 ":0"（随机端口，经 httpServer.Addr() 取
// 实际值）。t.Cleanup 自动走生产同源的关停序列。
func TestHelloEndpoint(t *testing.T) {
	app := lynxtest.Run(t, Setup, lynxtest.WithConfigMap(map[string]any{
		"service.name": "testing-example-test",
		"http.addr":    ":0",
	}))
	if got := app.Config().GetString("http.addr"); got != ":0" {
		t.Fatalf("http.addr = %q, want :0", got)
	}

	client := lynxtest.HTTPClient(t, httpServer)
	resp, err := client.Get("http://" + httpServer.Addr() + "/hello?name=lynx")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "hello lynx" {
		t.Fatalf("GET /hello = %d %q, want %d %q", resp.StatusCode, string(body), http.StatusOK, "hello lynx")
	}
}

// TestHealthEndpoint 验证健康端点与 app 级检查器聚合的接线。
func TestHealthEndpoint(t *testing.T) {
	_ = lynxtest.Run(t, Setup, lynxtest.WithConfigMap(map[string]any{
		"service.name": "testing-example-test",
		"http.addr":    ":0",
	}))

	client := lynxtest.HTTPClient(t, httpServer)
	resp, err := client.Get("http://" + httpServer.Addr() + "/healthz/liveness")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz/liveness = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
