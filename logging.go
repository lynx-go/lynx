package lynx

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler is a slog.Handler decorator that injects trace_id and span_id
// from the log call's context when it carries a valid SpanContext.
type traceHandler struct {
	next slog.Handler
}

// NewTraceHandler returns a slog.Handler that wraps h, automatically adding
// trace_id and span_id attributes to records logged with a context that
// carries a valid OpenTelemetry SpanContext. Wrap both plain slog handlers
// and zap-backed slog handlers (contrib/zap) with it for consistent fields.
func NewTraceHandler(h slog.Handler) slog.Handler {
	return traceHandler{next: h}
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{next: h.next.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{next: h.next.WithGroup(name)}
}
