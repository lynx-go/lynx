package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
