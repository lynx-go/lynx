package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func newRecordingHandler() (*bytes.Buffer, slog.Handler) {
	buf := &bytes.Buffer{}
	return buf, slog.NewJSONHandler(buf, nil)
}

func validSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
		TraceFlags: trace.FlagsSampled,
	})
}

func TestTraceHandlerAddsTraceContext(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewTraceHandler(base)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger := slog.New(h)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"trace_id":"`+sc.TraceID().String()+`"`) {
		t.Errorf("log output missing trace_id, got: %s", out)
	}
	if !strings.Contains(out, `"span_id":"`+sc.SpanID().String()+`"`) {
		t.Errorf("log output missing span_id, got: %s", out)
	}
}

func TestTraceHandlerWithoutSpan(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewTraceHandler(base))
	logger.InfoContext(context.Background(), "hello")

	out := buf.String()
	if strings.Contains(out, "trace_id") || strings.Contains(out, "span_id") {
		t.Errorf("log output should not contain trace fields, got: %s", out)
	}
}

func TestTraceHandlerInvalidSpanContext(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewTraceHandler(base))
	// Zero SpanContext is not valid; no fields should be added.
	ctx := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if strings.Contains(out, "trace_id") || strings.Contains(out, "span_id") {
		t.Errorf("invalid span context should not add trace fields, got: %s", out)
	}
}

func TestTraceHandlerWithAttrsKeepsDecoration(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewTraceHandler(base).WithAttrs([]slog.Attr{slog.String("component", "test")})
	logger := slog.New(h)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"trace_id"`) {
		t.Errorf("trace_id lost after WithAttrs, got: %s", out)
	}
	if !strings.Contains(out, `"component":"test"`) {
		t.Errorf("WithAttrs attribute missing, got: %s", out)
	}
}

func TestTraceHandlerWithGroupKeepsDecoration(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewTraceHandler(base).WithGroup("g")
	logger := slog.New(h)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	// The trace fields must be nested inside the group, not outside it.
	if !strings.Contains(out, `"g":{"trace_id":"`+sc.TraceID().String()+`"`) {
		t.Errorf("trace_id lost or not nested in group after WithGroup, got: %s", out)
	}
}

func TestTraceHandlerEnabledDelegates(t *testing.T) {
	base := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewTraceHandler(base)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled should delegate to the wrapped handler (Info disabled at Warn level)")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled should delegate to the wrapped handler (Error enabled at Warn level)")
	}
}

func TestWithAttrsMergesAndOverrides(t *testing.T) {
	ctx := context.Background()
	ctx = WithAttrs(ctx, slog.String("request_id", "rid-1"), slog.String("user_id", "u1"))
	ctx = WithAttrs(ctx, slog.String("user_id", "u2"), slog.String("lang", "zh"))

	attrs := AttrsFrom(ctx)
	if len(attrs) != 3 {
		t.Fatalf("got %d attrs, want 3: %v", len(attrs), attrs)
	}
	got := map[string]string{}
	for _, a := range attrs {
		got[a.Key] = a.Value.String()
	}
	if got["request_id"] != "rid-1" {
		t.Errorf("request_id = %q, want rid-1", got["request_id"])
	}
	if got["user_id"] != "u2" {
		t.Errorf("user_id = %q, want u2 (later write wins)", got["user_id"])
	}
	if got["lang"] != "zh" {
		t.Errorf("lang = %q, want zh", got["lang"])
	}
}

func TestWithAttrsEmptyReturnsSameCtx(t *testing.T) {
	ctx := context.Background()
	if WithAttrs(ctx) != ctx {
		t.Error("WithAttrs() with no attrs should return the same context")
	}
}

// TestWithAttrsDedupesWithinBatch 回归 AUX-07：同一批次内重复 key 不
// 去重时"最新写入为准"的承诺不成立（重复项会原样下发，部分 handler
// 产出重复 JSON 键）。批次内应保留最后一次传入的值（与跨调用"最新
// 覆盖"方向一致）。
func TestWithAttrsDedupesWithinBatch(t *testing.T) {
	ctx := WithAttrs(context.Background(),
		slog.String("k", "first"),
		slog.String("j", "keep"),
		slog.String("k", "last"),
	)

	attrs := AttrsFrom(ctx)
	if len(attrs) != 2 {
		t.Fatalf("got %d attrs, want 2 (duplicate k should be dropped): %v", len(attrs), attrs)
	}
	got := map[string]string{}
	for _, a := range attrs {
		got[a.Key] = a.Value.String()
	}
	if got["k"] != "last" {
		t.Errorf("k = %q, want last (later write wins within batch)", got["k"])
	}
	if got["j"] != "keep" {
		t.Errorf("j = %q, want keep (non-duplicate order preserved)", got["j"])
	}
}

// TestAttrsHandlerNoDuplicateKeysInOutput 验证去重后的属性在最终日志
// 输出中只出现一次（JSON 键不重复）。
func TestAttrsHandlerNoDuplicateKeysInOutput(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewAttrsHandler(base))

	ctx := WithAttrs(context.Background(),
		slog.String("request_id", "rid-old"),
		slog.String("request_id", "rid-new"),
	)
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if n := strings.Count(out, `"request_id":`); n != 1 {
		t.Errorf("request_id appears %d times in output, want 1: %s", n, out)
	}
	if !strings.Contains(out, `"request_id":"rid-new"`) {
		t.Errorf("output should keep the later value, got: %s", out)
	}
}

func TestAttrsFromUnset(t *testing.T) {
	if attrs := AttrsFrom(context.Background()); attrs != nil {
		t.Errorf("AttrsFrom on plain context = %v, want nil", attrs)
	}
}

func TestAttrsHandlerInjectsContextAttrs(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewAttrsHandler(base))

	ctx := WithAttrs(context.Background(),
		slog.String("request_id", "rid-1"), slog.String("user_id", "u1"))
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	for _, want := range []string{`"request_id":"rid-1"`, `"user_id":"u1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %s, got: %s", want, out)
		}
	}
}

func TestAttrsHandlerWithoutContextAttrs(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewAttrsHandler(base))
	logger.InfoContext(context.Background(), "hello")

	out := buf.String()
	if strings.Contains(out, "request_id") || strings.Contains(out, "user_id") {
		t.Errorf("log output should not contain request attrs, got: %s", out)
	}
}

func TestAttrsHandlerComposesWithTraceHandler(t *testing.T) {
	buf, base := newRecordingHandler()
	logger := slog.New(NewAttrsHandler(NewTraceHandler(base)))

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = WithAttrs(ctx, slog.String("request_id", "rid-1"))
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"trace_id":"`+sc.TraceID().String()+`"`) {
		t.Errorf("log output missing trace_id, got: %s", out)
	}
	if !strings.Contains(out, `"request_id":"rid-1"`) {
		t.Errorf("log output missing request_id, got: %s", out)
	}
}

func TestAttrsHandlerWithAttrsKeepsDecoration(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewAttrsHandler(base).WithAttrs([]slog.Attr{slog.String("component", "test")})
	logger := slog.New(h)

	ctx := WithAttrs(context.Background(), slog.String("request_id", "rid-1"))
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"request_id":"rid-1"`) {
		t.Errorf("request_id lost after WithAttrs, got: %s", out)
	}
	if !strings.Contains(out, `"component":"test"`) {
		t.Errorf("WithAttrs attribute missing, got: %s", out)
	}
}

func TestAttrsHandlerWithGroupKeepsDecoration(t *testing.T) {
	buf, base := newRecordingHandler()
	h := NewAttrsHandler(base).WithGroup("g")
	logger := slog.New(h)

	ctx := WithAttrs(context.Background(), slog.String("request_id", "rid-1"))
	logger.InfoContext(ctx, "hello")

	out := buf.String()
	if !strings.Contains(out, `"g":{"request_id":"rid-1"`) {
		t.Errorf("request_id lost or not nested in group after WithGroup, got: %s", out)
	}
}

func TestAttrsHandlerEnabledDelegates(t *testing.T) {
	base := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewAttrsHandler(base)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled should delegate to the wrapped handler (Info disabled at Warn level)")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled should delegate to the wrapped handler (Error enabled at Warn level)")
	}
}
