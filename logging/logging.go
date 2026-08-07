// Package logging 提供框架的日志机制：slog.Handler 装饰器族与
// 请求级日志属性的上下文传播（request_id/user_id 等全链路字段）。
package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler 是 slog.Handler 装饰器，日志调用的 Context 携带有效
// SpanContext 时，为记录注入 trace_id 与 span_id。
type traceHandler struct {
	next slog.Handler
}

// NewTraceHandler 返回包装 h 的 slog.Handler：log 调用的 Context 携带
// 有效的 OpenTelemetry SpanContext 时，自动追加 trace_id 与 span_id。
// 同时包装纯 slog handler 与 zap 桥接的 slog handler（contrib/zap）。
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

type attrsKeyCtx struct{}

func (attrsKeyCtx) String() string { return "logging attrs" }

// 全链路日志字段的标准 key：HTTP 中间件、pubsub 传播等机制以这些 key
// 读写日志属性，跨机制保持命名一致。
const (
	// FieldRequestID 是 request_id 日志属性 key。
	FieldRequestID = "request_id"
	// FieldUserID 是 user_id 日志属性 key。
	FieldUserID = "user_id"
)

// WithAttrs 将请求级日志属性写入 ctx，供 NewAttrsHandler 注入到该
// 上下文范围内的每条日志记录。同 key 的属性以最新写入为准（覆盖旧值）。
// 写入空 attrs 时原样返回 ctx。
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	seen := make(map[string]struct{}, len(attrs))
	for i := len(attrs) - 1; i >= 0; i-- {
		seen[attrs[i].Key] = struct{}{}
	}
	merged := make([]slog.Attr, 0, len(AttrsFrom(ctx))+len(attrs))
	for _, a := range AttrsFrom(ctx) {
		if _, ok := seen[a.Key]; ok {
			continue
		}
		merged = append(merged, a)
	}
	merged = append(merged, attrs...)
	return context.WithValue(ctx, attrsKeyCtx{}, merged)
}

// AttrsFrom 返回 ctx 中存储的请求级日志属性，未设置时返回 nil。
func AttrsFrom(ctx context.Context) []slog.Attr {
	v, _ := ctx.Value(attrsKeyCtx{}).([]slog.Attr)
	return v
}

// attrsHandler 是 slog.Handler 装饰器：日志调用的 Context 携带请求级
// 属性（WithAttrs 写入）时，为记录注入这些属性。
type attrsHandler struct {
	next slog.Handler
}

// NewAttrsHandler 返回包装 h 的 slog.Handler：log 调用的 Context 携带
// 请求级属性（WithAttrs 写入）时，自动为记录追加这些属性。
// 与 NewTraceHandler 组合使用：NewAttrsHandler(NewTraceHandler(base))。
func NewAttrsHandler(h slog.Handler) slog.Handler {
	return attrsHandler{next: h}
}

func (h attrsHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h attrsHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, a := range AttrsFrom(ctx) {
		r.AddAttrs(a)
	}
	return h.next.Handle(ctx, r)
}

func (h attrsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return attrsHandler{next: h.next.WithAttrs(attrs)}
}

func (h attrsHandler) WithGroup(name string) slog.Handler {
	return attrsHandler{next: h.next.WithGroup(name)}
}
