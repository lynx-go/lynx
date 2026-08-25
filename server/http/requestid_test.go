package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lynx-go/lynx/logging"
)

func TestWithRequestIDGeneratesAndPropagates(t *testing.T) {
	var gotRID string
	var gotAttrs []slog.Attr
	handler := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRID = RequestIDFrom(r.Context())
		gotAttrs = logging.AttrsFrom(r.Context())
	}), []Middleware{WithRequestID()})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if gotRID == "" {
		t.Error("RequestIDFrom(ctx) = empty, want generated id")
	}
	if got := rec.Header().Get(RequestIDHeader); got != gotRID {
		t.Errorf("response header %s = %q, want %q", RequestIDHeader, got, gotRID)
	}
	found := false
	for _, a := range gotAttrs {
		if a.Key == logging.FieldRequestID && a.Value.String() == gotRID {
			found = true
		}
	}
	if !found {
		t.Errorf("request_id log attr missing from ctx, attrs = %v", gotAttrs)
	}
}

func TestWithRequestIDReusesIncomingHeader(t *testing.T) {
	handler := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		[]Middleware{WithRequestID()})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "rid-from-client")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != "rid-from-client" {
		t.Errorf("response header %s = %q, want %q", RequestIDHeader, got, "rid-from-client")
	}
}

// entryRecorder 记录最近一次 request log Entry。
type entryRecorder struct {
	ent *Entry
}

func (r *entryRecorder) Log(e *Entry) { r.ent = e }

// TestRequestLogCapturesRequestID 验证访问日志 Entry 携带 request_id
// （从响应头读取，WithRequestID 中间件写入）。
func TestRequestLogCapturesRequestID(t *testing.T) {
	rec := &entryRecorder{}
	inner := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), []Middleware{WithRequestID()})
	h := NewHandler(rec, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "rid-in-request")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if rec.ent == nil {
		t.Fatal("no entry recorded")
	}
	if rec.ent.RequestID != "rid-in-request" {
		t.Errorf("Entry.RequestID = %q, want %q", rec.ent.RequestID, "rid-in-request")
	}
}

// TestRequestLogCapturesGeneratedRequestID 验证未带 X-Request-Id 的请求
// 生成的 request_id 同样进入访问日志。
func TestRequestLogCapturesGeneratedRequestID(t *testing.T) {
	rec := &entryRecorder{}
	inner := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), []Middleware{WithRequestID()})
	h := NewHandler(rec, inner)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.ent == nil {
		t.Fatal("no entry recorded")
	}
	if rec.ent.RequestID == "" {
		t.Error("Entry.RequestID = empty, want generated id")
	}
}

// TestRequestLogNoRequestID 验证未注册 WithRequestID 时访问日志不产生
// requestId 字段（保持兼容）。
func TestRequestLogNoRequestID(t *testing.T) {
	rec := &entryRecorder{}
	h := NewHandler(rec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.ent == nil {
		t.Fatal("no entry recorded")
	}
	if rec.ent.RequestID != "" {
		t.Errorf("Entry.RequestID = %q, want empty without WithRequestID", rec.ent.RequestID)
	}
}

// TestRequestIDFromUnset 验证未注入时 RequestIDFrom 返回空串。
func TestRequestIDFromUnset(t *testing.T) {
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Errorf("RequestIDFrom(plain ctx) = %q, want empty", got)
	}
}

// TestValidRequestID 单元测试 SC-22 的入站 request_id 校验：合法值
// （UUID/常见追踪 ID）通过，超长与非法字符被拒。
func TestValidRequestID(t *testing.T) {
	valid := []string{
		"rid-from-client",
		"0123456789abcdef",
		"aBcD-_1234",
		strings.Repeat("a", maxRequestIDLength),
	}
	for _, v := range valid {
		if !validRequestID(v) {
			t.Errorf("validRequestID(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", maxRequestIDLength+1), // 超长
		"rid with space",
		"rid\twith\ttab",
		"rid<>script", // 注入载荷
		"中文rid",
	}
	for _, v := range invalid {
		if validRequestID(v) {
			t.Errorf("validRequestID(%q) = true, want false", v)
		}
	}
}

// TestWithRequestIDRegeneratesInvalidHeader 回归 SC-22：入站
// X-Request-Id 超长或含非法字符时不透传——重新生成合法 ID 并回写。
func TestWithRequestIDRegeneratesInvalidHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"overlong", strings.Repeat("x", maxRequestIDLength+1)},
		{"illegal chars", "rid<>injection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotRID string
			handler := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotRID = RequestIDFrom(r.Context())
			}), []Middleware{WithRequestID()})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(RequestIDHeader, tc.in)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if gotRID == tc.in {
				t.Errorf("非法入站 request_id 被原样透传（SC-22）: %q", gotRID)
			}
			if !validRequestID(gotRID) {
				t.Errorf("重新生成的 request_id 非法: %q", gotRID)
			}
			if got := rec.Header().Get(RequestIDHeader); got != gotRID {
				t.Errorf("响应头 X-Request-Id = %q, want 重新生成的 %q", got, gotRID)
			}
		})
	}
}

// TestRequestLogEntryStats 回归 SC-09：去 Clone 后 Entry 的统计行为
// 不变——请求/响应 header 尺寸正确统计、响应体尺寸按实际字节数、
// Entry.Request.Body 恒为 nil（日志侧不持有请求体）。
func TestRequestLogEntryStats(t *testing.T) {
	rec := &entryRecorder{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 读空请求体：RequestBodySize 计入
		w.Header().Set("X-Resp", "v")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})
	h := NewHandler(rec, inner)

	req := httptest.NewRequest(http.MethodPost, "/stats", strings.NewReader("reqbody"))
	req.Header.Set("X-Test", "v")
	h.ServeHTTP(httptest.NewRecorder(), req)

	ent := rec.ent
	if ent == nil {
		t.Fatal("no entry recorded")
	}
	if got, want := ent.RequestHeaderSize, headerSize(req.Header); got != want {
		t.Errorf("RequestHeaderSize = %d, want %d", got, want)
	}
	if ent.RequestHeaderSize <= 0 {
		t.Error("RequestHeaderSize should count header bytes")
	}
	if ent.ResponseBodySize != 5 {
		t.Errorf("ResponseBodySize = %d, want 5（\"hello\"）", ent.ResponseBodySize)
	}
	if ent.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", ent.Status)
	}
	if ent.Request.Body != nil {
		t.Error("Entry.Request.Body 必须恒为 nil（SC-09 浅拷贝仍需隔离 body）")
	}
	if ent.RequestBodySize != int64(len("reqbody")) {
		t.Errorf("RequestBodySize = %d, want %d", ent.RequestBodySize, len("reqbody"))
	}
}
