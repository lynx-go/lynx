package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRecoveryPanicWritesResponse：panic handler → 500 + 通用 JSON 错误体
// （SC-04：panic 值不回传客户端）+ Error 日志（含 panic 与 stack 字段）；
// 连接不被毁掉，后续请求正常。
func TestRecoveryPanicWritesResponse(t *testing.T) {
	capture, restore := useCaptureLogger(false)
	defer restore()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("database exploded")
		}
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(Recovery()(handler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatalf("GET /boom error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(string(body)); got != `{"error":{"message":"Internal Server Error"}}` {
		t.Errorf("body = %q, want 通用消息（panic 值不得泄露, SC-04）", got)
	}
	if strings.Contains(string(body), "database exploded") {
		t.Error("panic 值泄露进响应体")
	}

	rec, ok := capture.hasError("http handler panic recovered")
	if !ok {
		t.Fatal("no Error log record for recovered panic")
	}
	if rec.attrs["panic"] != "database exploded" {
		t.Errorf("log panic = %q, want %q", rec.attrs["panic"], "database exploded")
	}
	if !strings.Contains(rec.attrs["stack"], "runtime/debug.Stack") {
		t.Errorf("log stack missing debug.Stack frame, got %q", rec.attrs["stack"])
	}

	resp, err = http.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("GET /ok after panic error = %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("request after panic: status = %d body = %q, want 200 ok", resp.StatusCode, body)
	}
}

// TestRecoveryCustomHandler：WithRecoveryHandler 覆盖缺省处理——自定义
// ErrorHandler 生效，panic 日志仍记录。
func TestRecoveryCustomHandler(t *testing.T) {
	capture, restore := useCaptureLogger(false)
	defer restore()

	h := Recovery(WithRecoveryHandler(func(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain")
		http.Error(w, "custom panic: "+err.Error(), http.StatusTeapot)
	}))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))

	rec := doRequest(h, http.MethodGet, "/")

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if !strings.Contains(rec.Body.String(), "custom panic: kaboom") {
		t.Errorf("body = %q, want custom panic body", rec.Body.String())
	}
	if _, ok := capture.hasError("http handler panic recovered"); !ok {
		t.Error("no panic log record with custom handler")
	}
}

// TestRecoveryConcurrent：-race 下并发 panic 无竞态，全部请求都有响应。
func TestRecoveryConcurrent(t *testing.T) {
	handler := Recovery()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("concurrent boom")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/ok"
			want := http.StatusOK
			if i%2 == 0 {
				path = "/boom"
				want = http.StatusInternalServerError
			}
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				errs <- fmt.Errorf("status = %d, want %d", resp.StatusCode, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
