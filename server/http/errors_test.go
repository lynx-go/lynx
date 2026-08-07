package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lynx-go/lynx/logging"
)

// notFoundError 实现 StatusError：声明 404。
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string   { return e.msg }
func (e *notFoundError) StatusCode() int { return http.StatusNotFound }

// wrappedStatusError 验证 errors.As 支持：错误包装后 StatusError 仍生效。
type wrappedStatusError struct{ err error }

func (e *wrappedStatusError) Error() string { return "wrapped: " + e.err.Error() }
func (e *wrappedStatusError) Unwrap() error { return e.err }

// capturedRecord 是捕获的 slog 记录（attr 摊平为 map 便于断言）。
type capturedRecord struct {
	msg   string
	level slog.Level
	attrs map[string]string
}

// captureHandler 收集全部 slog 记录，测试用断言日志内容。
type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := capturedRecord{msg: r.Message, level: r.Level, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) hasError(msg string) (capturedRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.level == slog.LevelError && r.msg == msg {
			return r, true
		}
	}
	return capturedRecord{}, false
}

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// useCaptureLogger 将 slog 默认日志器替换为捕获器，返回恢复函数。
func useCaptureLogger(wrapAttrs bool) (*captureHandler, func()) {
	h := &captureHandler{}
	var base slog.Handler = h
	if wrapAttrs {
		base = logging.NewAttrsHandler(base)
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(base))
	return h, func() { slog.SetDefault(prev) }
}

func doRequest(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestDefaultErrorHandlerStatusError：StatusError(404) → 404 + JSON 错误体，
// 且非 5xx 不记日志。
func TestDefaultErrorHandlerStatusError(t *testing.T) {
	capture, restore := useCaptureLogger(false)
	defer restore()

	h := NewErrorHandler(nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		return &notFoundError{msg: "user not found"}
	})
	rec := doRequest(h, http.MethodGet, "/users/42")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	wantBody := `{"error":{"message":"user not found"}}`
	if got := strings.TrimSpace(rec.Body.String()); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if n := capture.count(); n != 0 {
		t.Errorf("logged %d records for 4xx, want none", n)
	}
}

// TestDefaultErrorHandlerWrappedStatusError：errors.As 支持——被包装的
// StatusError 仍生效。
func TestDefaultErrorHandlerWrappedStatusError(t *testing.T) {
	h := NewErrorHandler(nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		return &wrappedStatusError{err: &notFoundError{msg: "user not found"}}
	})
	rec := doRequest(h, http.MethodGet, "/users/42")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	// errors.As 只取状态码；消息仍为原错误（包装层）的 Error()。
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":{"message":"wrapped: user not found"}}` {
		t.Errorf("body = %q, want wrapped error message", got)
	}
}

// TestDefaultErrorHandlerPlainError：普通 error → 500 + JSON 错误体 +
// Error 日志（method/path/status/error 四字段）。
func TestDefaultErrorHandlerPlainError(t *testing.T) {
	capture, restore := useCaptureLogger(false)
	defer restore()

	h := NewErrorHandler(nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		return io.EOF
	})
	rec := doRequest(h, http.MethodPost, "/orders")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":{"message":"EOF"}}` {
		t.Errorf("body = %q, want EOF JSON body", got)
	}

	rec2, ok := capture.hasError("http handler error")
	if !ok {
		t.Fatal("no Error log record for 5xx response")
	}
	if rec2.attrs["method"] != http.MethodPost {
		t.Errorf("log method = %q, want %q", rec2.attrs["method"], http.MethodPost)
	}
	if rec2.attrs["path"] != "/orders" {
		t.Errorf("log path = %q, want /orders", rec2.attrs["path"])
	}
	if rec2.attrs["status"] != "500" {
		t.Errorf("log status = %q, want 500", rec2.attrs["status"])
	}
	if rec2.attrs["error"] != "EOF" {
		t.Errorf("log error = %q, want EOF", rec2.attrs["error"])
	}
}

// TestDefaultErrorHandlerLogCarriesRequestAttrs：错误日志经 r.Context()
// 携带 request_id 等请求级属性（logging.NewAttrsHandler 注入）。
func TestDefaultErrorHandlerLogCarriesRequestAttrs(t *testing.T) {
	capture, restore := useCaptureLogger(true)
	defer restore()

	req := httptest.NewRequest(http.MethodGet, "/orders/1", nil)
	req = req.WithContext(logging.WithAttrs(req.Context(),
		slog.String(logging.FieldRequestID, "rid-123")))
	rec := httptest.NewRecorder()

	NewErrorHandler(nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		return io.EOF
	}).ServeHTTP(rec, req)

	rec2, ok := capture.hasError("http handler error")
	if !ok {
		t.Fatal("no Error log record for 5xx response")
	}
	if rec2.attrs[logging.FieldRequestID] != "rid-123" {
		t.Errorf("log request_id = %q, want rid-123", rec2.attrs[logging.FieldRequestID])
	}
}

// TestNewErrorHandlerNilReturn：fn 返回 nil → 不调用 ErrorHandler，
// 响应由 fn 自行写出。
func TestNewErrorHandlerNilReturn(t *testing.T) {
	called := false
	h := NewErrorHandler(func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
		called = true
	}, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		_, _ = w.Write([]byte("ok"))
		return nil
	})
	rec := doRequest(h, http.MethodGet, "/")

	if called {
		t.Error("ErrorHandler was called for nil error")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
}

// TestNewErrorHandlerCustomHandler：自定义 ErrorHandler 生效。
func TestNewErrorHandlerCustomHandler(t *testing.T) {
	h := NewErrorHandler(func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "custom: "+err.Error(), http.StatusTeapot)
	}, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		return io.EOF
	})
	rec := doRequest(h, http.MethodGet, "/")

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if !strings.Contains(rec.Body.String(), "custom: EOF") {
		t.Errorf("body = %q, want custom error body", rec.Body.String())
	}
}

// TestNewErrorHandlerHeaderAlreadyWritten：fn 已写响应头/体后再返回错误
// → 不改写响应（不触发 superfluous WriteHeader），仅记 Error 日志。
func TestNewErrorHandlerHeaderAlreadyWritten(t *testing.T) {
	capture, restore := useCaptureLogger(false)
	defer restore()

	h := NewErrorHandler(nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		return io.EOF
	})
	rec := doRequest(h, http.MethodGet, "/stream")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (response must not be rewritten)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "partial" {
		t.Errorf("body = %q, want %q (error body must not be appended)", got, "partial")
	}

	rec2, ok := capture.hasError("http handler error after response started")
	if !ok {
		t.Fatal("no Error log record for error after response started")
	}
	if rec2.attrs["status"] != "200" {
		t.Errorf("log status = %q, want 200 (status already written)", rec2.attrs["status"])
	}
	if rec2.attrs["error"] != "EOF" {
		t.Errorf("log error = %q, want EOF", rec2.attrs["error"])
	}
}

// TestNewErrorHandlerFullChain：httptest 全链路——真实 HTTP 往返，错误
// 经 NewErrorHandler → DefaultErrorHandler → 客户端。
func TestNewErrorHandlerFullChain(t *testing.T) {
	srv := httptest.NewServer(NewErrorHandler(nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path != "/users/42" {
			return &notFoundError{msg: "user not found"}
		}
		_, _ = w.Write([]byte("hello user"))
		return nil
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/users/42")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello user" {
		t.Errorf("success path: status = %d body = %q, want 200 hello user", resp.StatusCode, body)
	}

	resp, err = http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("error path: status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("error path: Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(string(body)); got != `{"error":{"message":"user not found"}}` {
		t.Errorf("error path: body = %q, want JSON error body", got)
	}
}
