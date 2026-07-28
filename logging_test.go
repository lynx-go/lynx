package lynx

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
