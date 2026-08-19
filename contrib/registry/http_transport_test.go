package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingTransport 记录最后一次收到的请求并返回固定响应。
type recordingTransport struct {
	mu   sync.Mutex
	last *http.Request
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.last = req
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    req,
	}, nil
}

func (rt *recordingTransport) lastRequest() *http.Request {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.last
}

// newHTTPFixture 返回注册了给定实例的 memory 后端、Resolver 与
// recordingTransport 包内层的 RoundTripper。
func newHTTPFixture(t *testing.T, insts ...Instance) (*Memory, http.RoundTripper, *recordingTransport) {
	t.Helper()
	m := NewMemory()
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()
	for _, inst := range insts {
		if err := m.Register(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	rslv := NewResolver(m)
	t.Cleanup(func() { _ = rslv.Close() })
	rec := &recordingTransport{}
	return m, NewHTTPTransport(rslv).Wrap(rec), rec
}

func do(t *testing.T, rt http.RoundTripper, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return req
}

func TestHTTPTransportPassthroughNonRegistryScheme(t *testing.T) {
	_, rt, rec := newHTTPFixture(t)
	// 非 registry scheme：原样交给内层 Transport，不做任何解析。
	req := do(t, rt, "http://example.com/users/1")
	got := rec.lastRequest()
	if got != req {
		t.Fatal("non-registry request must be passed through as-is")
	}
	if got.URL.Scheme != "http" || got.URL.Host != "example.com" {
		t.Fatalf("passthrough changed URL: %s", got.URL)
	}
}

func TestHTTPTransportCloneAndReResolvePerCall(t *testing.T) {
	_, rt, rec := newHTTPFixture(t,
		passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}),
		passing("svc", "i2", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.2:8080"}),
	)

	// 连续 RoundTrip 复用同一 request：每次都重新解析（默认 RoundRobin
	// 在缓存快照上轮换），调用方 URL 不被改写。打 4 次：缓存快照可能
	// 被 watch 首轮推送替换为等值但顺序不同的切片，4 次调用无论切片
	// 顺序与轮换奇偶如何都必然覆盖两个实例。
	req, err := http.NewRequest(http.MethodGet, "registry://svc/users/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	for i := 0; i < 4; i++ {
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		hosts[rec.lastRequest().URL.Host] = true
	}
	if len(hosts) != 2 {
		t.Fatalf("round-robin should hit 2 instances, got %v", hosts)
	}
	if req.URL.Scheme != "registry" || req.URL.Host != "svc" {
		t.Fatalf("caller URL must not be rewritten, got %s", req.URL)
	}
	if rec.lastRequest().URL.Path != "/users/1" {
		t.Fatalf("path must be preserved, got %q", rec.lastRequest().URL.Path)
	}
}

func TestHTTPTransportProtocolReservedKeyStripped(t *testing.T) {
	_, rt, rec := newHTTPFixture(t,
		passing("svc", "i1", Endpoint{Protocol: ProtocolHTTPS, Address: "10.0.0.1:8443"}),
	)
	do(t, rt, "registry://svc/users?id=1&protocol=https")
	got := rec.lastRequest()
	if got.URL.Scheme != "https" {
		t.Fatalf("want https scheme, got %q", got.URL.Scheme)
	}
	// 保留键 protocol 不得漏给业务 query。
	if got.URL.Query().Get("protocol") != "" {
		t.Fatalf("reserved key leaked: %q", got.URL.RawQuery)
	}
	if got.URL.Query().Get("id") != "1" {
		t.Fatalf("business query lost: %q", got.URL.RawQuery)
	}
}

func TestHTTPTransportBadProtocol(t *testing.T) {
	_, rt, _ := newHTTPFixture(t,
		passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}),
	)
	req, err := http.NewRequest(http.MethodGet, "registry://svc/users?protocol=grpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(req); !errors.Is(err, ErrBadProtocol) {
		t.Fatalf("want ErrBadProtocol, got %v", err)
	}
}

func TestHTTPTransportHTTPSFallback(t *testing.T) {
	// 只有 https Endpoint：未指定 protocol 时回落第一条 https（稳定顺序）。
	_, rt, rec := newHTTPFixture(t,
		passing("svc", "i1",
			Endpoint{Protocol: ProtocolHTTPS, Address: "10.0.0.1:8443"},
			Endpoint{Protocol: ProtocolHTTPS, Address: "10.0.0.1:9443"}),
	)
	do(t, rt, "registry://svc/users")
	got := rec.lastRequest()
	if got.URL.Scheme != "https" || got.URL.Host != "10.0.0.1:8443" {
		t.Fatalf("https fallback: got %s", got.URL)
	}
}

func TestHTTPTransportPrefersHTTPOverHTTPS(t *testing.T) {
	// 两者都有：只用 http，避免明文/TLS 混选。
	_, rt, rec := newHTTPFixture(t,
		passing("svc", "i1",
			Endpoint{Protocol: ProtocolHTTPS, Address: "10.0.0.1:8443"},
			Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}),
	)
	do(t, rt, "registry://svc/users")
	got := rec.lastRequest()
	if got.URL.Scheme != "http" || got.URL.Host != "10.0.0.1:8080" {
		t.Fatalf("must prefer http endpoint, got %s", got.URL)
	}
}

func TestHTTPTransportHostHeader(t *testing.T) {
	_, rt, rec := newHTTPFixture(t,
		passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}),
	)
	// 未显式设 Host（http.NewRequest 预填为 URL.Host）：改写为 Endpoint.Address。
	do(t, rt, "registry://svc/users")
	if got := rec.lastRequest().Host; got != "10.0.0.1:8080" {
		t.Fatalf("Host header should follow rewritten URL.Host, got %q", got)
	}
	// 调用方显式设置成其它值的 Host 保留。
	req, err := http.NewRequest(http.MethodGet, "registry://svc/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "custom.example.com"
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := rec.lastRequest().Host; got != "custom.example.com" {
		t.Fatalf("caller Host must be preserved, got %q", got)
	}
}

func TestHTTPTransportNoInstance(t *testing.T) {
	_, rt, _ := newHTTPFixture(t)
	req, err := http.NewRequest(http.MethodGet, "registry://ghost/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(req); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("want wrapped ErrNoInstance, got %v", err)
	}
}

func TestHTTPTransportEmptyServiceName(t *testing.T) {
	_, rt, _ := newHTTPFixture(t)
	req, err := http.NewRequest(http.MethodGet, "registry:///users", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(req); !errors.Is(err, ErrBadName) {
		t.Fatalf("want ErrBadName, got %v", err)
	}
}

func TestHTTPTransportWrapNilBase(t *testing.T) {
	// Wrap(nil) 退到 http.DefaultTransport；结合 httptest 做真实拨号。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	m := NewMemory()
	defer func() { _ = m.Close() }()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := m.Register(context.Background(),
		passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: addr})); err != nil {
		t.Fatal(err)
	}
	rslv := NewResolver(m)
	defer func() { _ = rslv.Close() }()

	rt := NewHTTPTransport(rslv).Wrap(nil)
	req, err := http.NewRequest(http.MethodGet, "registry://svc/teapot", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("want 418, got %d", resp.StatusCode)
	}
}
