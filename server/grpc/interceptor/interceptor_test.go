package interceptor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureHandler 收集 slog 记录，用于断言拦截器日志内容与级别。
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// findByMsg 返回首条匹配消息的记录。
func (h *captureHandler) findByMsg(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

func (h *captureHandler) attrValue(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
		}
		return true
	})
	return out
}

func TestLoggingPassthrough(t *testing.T) {
	interceptor := Logging(testLogger())
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "resp", nil
	})
	if err != nil {
		t.Fatalf("interceptor error = %v, want nil", err)
	}
	if resp != "resp" {
		t.Errorf("resp = %v, want %q", resp, "resp")
	}
}

func TestLoggingHandlerError(t *testing.T) {
	interceptor := Logging(testLogger())
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	wantErr := errors.New("handler failed")
	_, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("interceptor error = %v, want %v", err, wantErr)
	}
}

// TestLoggingWithLevel 验证 SC-07 的级别选项：Debug 级配置下记录落在
// Debug 而非 Info。
func TestLoggingWithLevel(t *testing.T) {
	h := &captureHandler{}
	interceptor := LoggingWithLevel(slog.New(h), slog.LevelDebug)
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	if _, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "resp", nil
	}); err != nil {
		t.Fatalf("interceptor error = %v, want nil", err)
	}
	rec, ok := h.findByMsg("gRPC request")
	if !ok {
		t.Fatal("no request log record")
	}
	if rec.Level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug", rec.Level)
	}
}

// TestLoggingDefaultLevelIsInfo 验证 Logging() 缺省保持 Info（兼容）。
func TestLoggingDefaultLevelIsInfo(t *testing.T) {
	h := &captureHandler{}
	interceptor := Logging(slog.New(h))
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	_, _ = interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, nil
	})
	rec, ok := h.findByMsg("gRPC request")
	if !ok {
		t.Fatal("no request log record")
	}
	if rec.Level != slog.LevelInfo {
		t.Errorf("level = %v, want Info", rec.Level)
	}
}

func TestRecoveryPassthrough(t *testing.T) {
	interceptor := Recovery()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "resp", nil
	})
	if err != nil {
		t.Fatalf("interceptor error = %v, want nil", err)
	}
	if resp != "resp" {
		t.Errorf("resp = %v, want %q", resp, "resp")
	}
}

// TestRecoveryRecoversPanic 回归 SC-04/SC-06：panic 被恢复并返回通用
// internal error（panic 值不回传客户端），panic 值与调用栈进日志。
func TestRecoveryRecoversPanic(t *testing.T) {
	h := &captureHandler{}
	interceptor := RecoveryWithLogger(slog.New(h))
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		panic("boom postgres://u:p@db/internal")
	})
	if err == nil {
		t.Fatal("interceptor error = nil, want recovered panic error")
	}
	if resp != nil {
		t.Errorf("resp = %v, want nil", resp)
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("status code = %v, want %v", code, codes.Internal)
	}
	// SC-04：对客户端只暴露通用消息，panic 值不得泄露。
	if msg := status.Convert(err).Message(); msg != "internal error" {
		t.Errorf("error message = %q, want generic %q（panic 值不得泄露, SC-04）", msg, "internal error")
	}
	// SC-06：本地留痕——panic 值与调用栈都在日志里。
	rec, ok := h.findByMsg("gRPC handler panic recovered")
	if !ok {
		t.Fatal("no panic log record（SC-06：panic 后应有本地痕迹）")
	}
	if got := h.attrValue(rec, "method"); got != "/test.Service/Method" {
		t.Errorf("log method = %q, want %q", got, "/test.Service/Method")
	}
	if got := h.attrValue(rec, "panic"); !strings.Contains(got, "boom") {
		t.Errorf("log panic = %q, want panic 值", got)
	}
	if got := h.attrValue(rec, "stack"); !strings.Contains(got, "interceptor.") {
		t.Errorf("log stack = %q, want 含 debug.Stack 调用栈", got)
	}
}

// fakeServerStream is a minimal grpc.ServerStream for testing stream interceptors.
type fakeServerStream struct {
	ctx context.Context
	grpc.ServerStream
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func TestLoggingStreamPassthrough(t *testing.T) {
	interceptor := LoggingStream(testLogger())
	info := &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}
	ss := &fakeServerStream{ctx: context.Background()}

	if err := interceptor(nil, ss, info, func(srv interface{}, stream grpc.ServerStream) error {
		return nil
	}); err != nil {
		t.Fatalf("interceptor error = %v, want nil", err)
	}
}

func TestLoggingStreamHandlerError(t *testing.T) {
	interceptor := LoggingStream(testLogger())
	info := &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}
	ss := &fakeServerStream{ctx: context.Background()}

	wantErr := errors.New("stream failed")
	if err := interceptor(nil, ss, info, func(srv interface{}, stream grpc.ServerStream) error {
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Errorf("interceptor error = %v, want %v", err, wantErr)
	}
}

// TestRecoveryStreamRecoversPanic 回归 SC-04/SC-06：流式 panic 恢复 +
// 通用错误 + 日志留痕。
func TestRecoveryStreamRecoversPanic(t *testing.T) {
	h := &captureHandler{}
	interceptor := RecoveryStreamWithLogger(slog.New(h))
	info := &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}
	ss := &fakeServerStream{ctx: context.Background()}

	err := interceptor(nil, ss, info, func(srv interface{}, stream grpc.ServerStream) error {
		panic("stream boom")
	})
	if err == nil {
		t.Fatal("interceptor error = nil, want recovered panic error")
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("status code = %v, want %v", code, codes.Internal)
	}
	if msg := status.Convert(err).Message(); msg != "internal error" {
		t.Errorf("error message = %q, want generic %q（SC-04）", msg, "internal error")
	}
	rec, ok := h.findByMsg("gRPC stream handler panic recovered")
	if !ok {
		t.Fatal("no stream panic log record（SC-06）")
	}
	if got := h.attrValue(rec, "panic"); !strings.Contains(got, "stream boom") {
		t.Errorf("log panic = %q, want panic 值", got)
	}
	if got := h.attrValue(rec, "stack"); !strings.Contains(got, "interceptor.") {
		t.Errorf("log stack = %q, want 调用栈", got)
	}
}
