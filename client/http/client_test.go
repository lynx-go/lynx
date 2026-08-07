package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/logging"
	serverhttp "github.com/lynx-go/lynx/server/http"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestPropagation 断言 ctx 中的日志属性被写入请求头，且 otel 插装
// 注入 traceparent（传播闭环的发送侧）。
func TestPropagation(t *testing.T) {
	var (
		mu             sync.Mutex
		gotRID, gotUID string
		gotTraceparent string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotRID = r.Header.Get(RequestIDHeader)
		gotUID = r.Header.Get(UserIDHeader)
		gotTraceparent = r.Header.Get("traceparent")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(
		WithTracerProvider(sdktrace.NewTracerProvider()),
		WithPropagator(propagation.TraceContext{}),
	)
	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "rid-1"),
		slog.String(logging.FieldUserID, "u1"),
	)
	resp, err := client.Get(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if gotRID != "rid-1" {
		t.Errorf("X-Request-Id = %q, want rid-1", gotRID)
	}
	if gotUID != "u1" {
		t.Errorf("X-User-Id = %q, want u1", gotUID)
	}
	if !strings.HasPrefix(gotTraceparent, "00-") {
		t.Errorf("traceparent = %q, want valid trace context", gotTraceparent)
	}
}

// TestPropagationRespectsExistingHeader 断言已存在的同名请求头不被覆盖。
func TestPropagationRespectsExistingHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(RequestIDHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New()
	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "auto-rid"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(RequestIDHeader, "explicit")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if got != "explicit" {
		t.Errorf("X-Request-Id = %q, want explicit（已存在头部不被覆盖）", got)
	}
}

// TestTimeout 断言整体超时生效：慢 server + WithTimeout(50ms) 报错。
func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(WithTimeout(50 * time.Millisecond))
	_, err := client.Get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestRetry 断言 503,503,200 重试序列：第三次成功且总尝试 3 次。
func TestRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(WithRetry(3,
		WithRetryInitialInterval(10*time.Millisecond),
		WithRetryMaxInterval(50*time.Millisecond)))
	resp, err := client.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestRetryReplayableBody 断言带可重放 body 的请求在重试时重建请求体：
// 每次尝试 server 都能读到完整 body。
func TestRetryReplayableBody(t *testing.T) {
	var (
		attempts atomic.Int32
		mu       sync.Mutex
		seen     []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(b))
		mu.Unlock()
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(WithRetry(3,
		WithRetryInitialInterval(10*time.Millisecond),
		WithRetryMaxInterval(50*time.Millisecond)))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody == nil {
		t.Fatal("strings.Reader 请求应可重放（GetBody 非 nil）")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("seen bodies = %d, want 3", len(seen))
	}
	for i, b := range seen {
		if b != "payload" {
			t.Errorf("attempt %d body = %q, want payload", i+1, b)
		}
	}
}

// TestRetryNonReplayableBody 断言不可重放 body 的请求只发送一次。
func TestRetryNonReplayableBody(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(WithRetry(3,
		WithRetryInitialInterval(10*time.Millisecond),
		WithRetryMaxInterval(50*time.Millisecond)))
	// 自定义 reader 的 body 不可重放（GetBody 为 nil）。
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(strings.NewReader("payload"))
	req.GetBody = nil
	req.ContentLength = 7

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503（不重试，返回最后尝试的响应）", resp.StatusCode)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1（不可重放 body 不重试）", got)
	}
}

// TestRetryRetryAfter 断言 429 响应的 Retry-After 被遵守：
// 重试前至少等待其指示的时长。
func TestRetryRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(WithRetry(2, WithRetryInitialInterval(10*time.Millisecond)))
	start := time.Now()
	resp, err := client.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 1s（Retry-After 未遵守）", elapsed)
	}
}

// failOnceRoundTripper 首次调用返回传输错误，之后透传给 next。
type failOnceRoundTripper struct {
	next   http.RoundTripper
	mu     sync.Mutex
	failed bool
}

func (f *failOnceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if !f.failed {
		f.failed = true
		f.mu.Unlock()
		return nil, errors.New("boom: transport error")
	}
	f.mu.Unlock()
	return f.next.RoundTrip(r)
}

// TestRetryTransportError 断言传输层错误触发重试。
func TestRetryTransportError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(
		WithRetry(3, WithRetryInitialInterval(10*time.Millisecond)),
		WithClientOptions(func(c *http.Client) {
			c.Transport = &failOnceRoundTripper{next: srv.Client().Transport}
		}),
	)
	resp, err := client.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("server attempts = %d, want 1（首次失败未到达 server）", got)
	}
}

// TestBodyLeftToCaller 断言 Do 不读取、不关闭响应体：调用方负责。
func TestBodyLeftToCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	client := New()
	resp, err := client.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("caller 读取 body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("caller 关闭 body: %v", err)
	}
	if string(b) != "hello" {
		t.Errorf("body = %q, want hello", b)
	}
}

// TestPropagationClosedLoop 是传播闭环的端到端测试：
// client 发送（ctx 预置 request_id）→ server/http WithRequestID 接收
// 并还原进 ctx，断言两端 request_id 一致。
func TestPropagationClosedLoop(t *testing.T) {
	var (
		mu    sync.Mutex
		gotID string
	)
	handler := serverhttp.WithRequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotID = serverhttp.RequestIDFrom(r.Context())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := New()
	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "rid-e2e"))
	resp, err := client.Get(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	mu.Lock()
	rid := gotID
	mu.Unlock()
	if rid != "rid-e2e" {
		t.Errorf("server 还原 request_id = %q, want rid-e2e", rid)
	}
	if got := resp.Header.Get(serverhttp.RequestIDHeader); got != "rid-e2e" {
		t.Errorf("响应头 X-Request-Id = %q, want rid-e2e", got)
	}
}

// TestRetryAfterParse 单元测试 Retry-After 解析：秒数、HTTP-date、
// 非 429/503 不读取、非法值返回 0。
func TestRetryAfterParse(t *testing.T) {
	sec := &http.Response{StatusCode: http.StatusTooManyRequests,
		Header: http.Header{"Retry-After": {"2"}}}
	if d := retryAfter(sec); d != 2*time.Second {
		t.Errorf("seconds form = %v, want 2s", d)
	}

	date := &http.Response{StatusCode: http.StatusServiceUnavailable,
		Header: http.Header{"Retry-After": {time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)}}}
	// HTTP-date 只有秒精度（截断最多丢 1s），断言落在 [2s, 4s]。
	if d := retryAfter(date); d < 2*time.Second || d > 4*time.Second {
		t.Errorf("date form = %v, want ~3s", d)
	}

	expired := &http.Response{StatusCode: http.StatusServiceUnavailable,
		Header: http.Header{"Retry-After": {time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)}}}
	if d := retryAfter(expired); d != 0 {
		t.Errorf("expired date = %v, want 0", d)
	}

	// 非 429/503（502）不读取 Retry-After。
	notApplicable := &http.Response{StatusCode: http.StatusBadGateway,
		Header: http.Header{"Retry-After": {"1"}}}
	if d := retryAfter(notApplicable); d != 0 {
		t.Errorf("502 Retry-After = %v, want 0", d)
	}

	invalid := &http.Response{StatusCode: http.StatusTooManyRequests,
		Header: http.Header{"Retry-After": {"garbage"}}}
	if d := retryAfter(invalid); d != 0 {
		t.Errorf("invalid = %v, want 0", d)
	}
}

// TestPropagateAttrsUnit 单元测试属性传播：仅 request_id/user_id 两个
// key 写入对应头部，已存在头部不被覆盖，其余属性被忽略。
func TestPropagateAttrsUnit(t *testing.T) {
	req := &http.Request{Header: http.Header{}}
	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "rid"),
		slog.String(logging.FieldUserID, "uid"),
		slog.String("other", "ignored"))
	req = req.WithContext(ctx)
	propagateAttrs(req)
	if got := req.Header.Get(RequestIDHeader); got != "rid" {
		t.Errorf("X-Request-Id = %q, want rid", got)
	}
	if got := req.Header.Get(UserIDHeader); got != "uid" {
		t.Errorf("X-User-Id = %q, want uid", got)
	}
	if _, ok := req.Header["Other"]; ok {
		t.Error("其他日志属性不应写入请求头")
	}

	req.Header.Set(RequestIDHeader, "explicit")
	propagateAttrs(req)
	if got := req.Header.Get(RequestIDHeader); got != "explicit" {
		t.Errorf("已存在 X-Request-Id = %q, want explicit", got)
	}
}
