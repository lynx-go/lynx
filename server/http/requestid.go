package http

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx/logging"
)

// RequestIDHeader 是 request_id 透传/回写的 HTTP 头部名。
const RequestIDHeader = "X-Request-Id"

// WithRequestID 返回一个中间件：为每个请求生成或透传 request_id——
// 请求头携带 X-Request-Id 时沿用，否则生成 UUID；回写响应头，并通过
// logging.WithAttrs 写入请求 ctx，使请求链内所有 InfoContext 日志
// 自动携带 request_id。建议作为 WithMiddleware 的第一个中间件注册。
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(RequestIDHeader)
			if rid == "" {
				rid = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, rid)
			r = r.WithContext(logging.WithAttrs(r.Context(),
				slog.String(logging.FieldRequestID, rid)))
			next.ServeHTTP(w, r)
		})
	}
}

// RequestIDFrom 返回请求 ctx 中的 request_id，未设置时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	for _, a := range logging.AttrsFrom(ctx) {
		if a.Key == logging.FieldRequestID {
			return a.Value.String()
		}
	}
	return ""
}
