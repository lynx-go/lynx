package http

import (
	"bytes"
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

// redirectRoundTripper 返回 302 响应：配合 CheckRedirect 报错，使标准库
// http.Client 同时返回非 nil 的 resp（body 已关闭）与 err——这是
// Client.Do 可能出现「err != nil 且 resp != nil」的真实可达路径。
type redirectRoundTripper struct{}

func (redirectRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": {"http://example.invalid/y"}},
		Body:       io.NopCloser(strings.NewReader("redirecting")),
		Request:    r,
	}, nil
}

// TestDoErrorReturnsRawBody 回归复审 G：Do 在 err != nil 时（即使同时带
// 非 nil resp，如 CheckRedirect 报错）必须立即释放整体超时 ctx 并原样
// 返回两者，不得把 body 包进 cancelBody——错误返回的 body 无人读取，
// 包装会让超时定时器存活到 deadline 自然到期（标准库 net/http 的
// Client.send 同款处理：err 非 nil 即无条件 stop 定时器）。
func TestDoErrorReturnsRawBody(t *testing.T) {
	client := New(
		WithTimeout(30*time.Second),
		WithClientOptions(func(c *http.Client) {
			c.Transport = redirectRoundTripper{}
			c.CheckRedirect = func(*http.Request, []*http.Request) error {
				return errors.New("no redirects")
			}
		}),
	)
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "no redirects") {
		t.Fatalf("Do err = %v, want redirect rejection", err)
	}
	if resp == nil {
		t.Fatal("redirect error must still return the previous response alongside err")
	}
	defer func() { _ = resp.Body.Close() }()
	// 错误路径不包装 body：resp.Body 即传输层返回的原始对象。
	if _, wrapped := resp.Body.(*cancelBody); wrapped {
		t.Fatalf("error path must not wrap body in cancelBody, got %T", resp.Body)
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

// TestLargeChunkedBodyReadableAfterDoReturns 回归 SC-01：整体超时不得在
// Do 返回时取消 ctx——否则连接被立即关闭，大响应/分块 body 读取必得
// context canceled。此处 256KB 分块（禁用内部缓冲、逐块 flush）响应在
// Do 返回后必须能被调用方完整读取。
func TestLargeChunkedBodyReadableAfterDoReturns(t *testing.T) {
	const chunkSize = 16 * 1024
	const chunks = 16 // 共 256KB，超过传输层缓冲，杜绝小响应掩盖问题
	chunk := bytes.Repeat([]byte("a"), chunkSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush() // 分块编码：Do 在 body 结束前就能返回
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	client := New(WithTimeout(5 * time.Second))
	resp, err := client.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取大响应 body 失败（SC-01 回归，ctx 被 Do 提前取消）: %v", err)
	}
	if want := chunkSize * chunks; len(b) != want {
		t.Errorf("body 长度 = %d, want %d", len(b), want)
	}
}

// TestSlowBodyReadFailsAfterTimeout 回归 SC-01 另一半：Do 返回后慢速
// body 读取仍受整体超时约束——超时到期后读取返回错误，不永久挂起。
// （与自身文档"ctx 到期后读取返回错误"的契约一致。）
func TestSlowBodyReadFailsAfterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Second) // 慢速 body：远超客户端整体超时
		_, _ = w.Write([]byte("more"))
	}))
	defer srv.Close()

	client := New(WithTimeout(200 * time.Millisecond))
	resp, err := client.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get（头部阶段应成功）: %v", err)
	}
	defer resp.Body.Close()

	start := time.Now()
	_, readErr := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if readErr == nil {
		t.Fatal("慢 body 读取在整体超时后仍成功，want 错误（超时应约束 body 读取）")
	}
	if elapsed > 3*time.Second {
		t.Errorf("读取耗时 %v，超时后应尽快返回错误", elapsed)
	}
}

// TestCapRetryWait 单元测试 SC-12：重试等待上限取 min(原值, 整体超时
// 剩余, 2min 固定上限)。
func TestCapRetryWait(t *testing.T) {
	// 无 deadline：仅受固定上限约束。
	if got := capRetryWait(context.Background(), time.Hour); got != 2*time.Minute {
		t.Errorf("无 deadline 时 1h 被 cap 为 %v, want 2m", got)
	}
	if got := capRetryWait(context.Background(), time.Second); got != time.Second {
		t.Errorf("小于上限的等待不应被改变: %v, want 1s", got)
	}

	// 有 deadline：受剩余时间约束。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := capRetryWait(ctx, time.Hour)
	if got <= 0 || got > 100*time.Millisecond {
		t.Errorf("有 deadline 时 1h 被 cap 为 %v, want (0, 100ms]", got)
	}
}

// TestRetryAfterExtremeValueCapped 回归 SC-12：对端返回 Retry-After:
// 86400 时不得等满——等待被钳制到整体超时剩余，超时后以
// DeadlineExceeded 返回，而不是挂死等待。
func TestRetryAfterExtremeValueCapped(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := New(WithTimeout(300*time.Millisecond),
		WithRetry(2, WithRetryInitialInterval(10*time.Millisecond)))
	start := time.Now()
	_, err := client.Get(context.Background(), srv.URL)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("elapsed = %v, 等待未被钳制（极端 Retry-After 被等满）", elapsed)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1（超时前不应发生第二次尝试）", got)
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
