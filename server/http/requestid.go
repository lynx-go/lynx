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

// maxRequestIDLength 是入站 X-Request-Id 的最大长度（SC-22）：超长或含
// 非法字符的值通常是异常客户端/攻击载荷，直接重新生成，不透传、不注入
// 日志（避免刷日志与污染下游）。
const maxRequestIDLength = 128

// WithRequestID 返回一个中间件：为每个请求生成或透传 request_id——
// 请求头携带合法的 X-Request-Id 时沿用（长度 ≤128 且字符集为
// [A-Za-z0-9-_]，非法值重新生成，SC-22），否则生成 UUID；回写响应头，
// 并通过 logging.WithAttrs 写入请求 ctx，使请求链内所有 InfoContext
// 日志自动携带 request_id。
//
// 注册顺序：建议紧随 Recovery 之后（推荐链 Recovery → RequestID →
// 其余中间件，SC-13）——Recovery 必须保持最外层保命，RequestID 在其
// 内侧的代价是 panic 日志拿不到 request_id，属已知取舍（详见 Recovery
// 的注释）。
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(RequestIDHeader)
			if !validRequestID(rid) {
				rid = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, rid)
			r = r.WithContext(logging.WithAttrs(r.Context(),
				slog.String(logging.FieldRequestID, rid)))
			next.ServeHTTP(w, r)
		})
	}
}

// validRequestID 判定入站 request_id 是否可安全沿用：非空、长度 ≤128、
// 字符集限定 [A-Za-z0-9-_]（UUID/常见追踪 ID 均落在该集合内）。
func validRequestID(rid string) bool {
	if rid == "" || len(rid) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(rid); i++ {
		c := rid[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
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
