package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx/internal/serverkit"
	"github.com/lynx-go/lynx/logging"
)

// RequestIDHeader 是 request_id 透传/回写的 HTTP 头部名：与 client/http
// 及 gRPC metadata 同源的共享键（internal/serverkit）。
const RequestIDHeader = serverkit.RequestIDKey

// UserIDHeader 是 user_id 透传/还原的 HTTP 头部名（与 gRPC metadata 同源）。
const UserIDHeader = serverkit.UserIDKey

// WithRequestID 返回一个中间件：为每个请求生成或透传 request_id——
// 请求头携带合法的 x-request-id 时沿用（长度 ≤128 且字符集为
// [A-Za-z0-9-_]，非法值重新生成），否则生成 UUID；回写响应头；user_id
// 合法时一并还原；两者经 logging.WithAttrs 写入请求 ctx，使请求链内所有
// InfoContext 日志自动携带。服务端默认安装（WithDisableRequestID 关闭）。
//
// 注册顺序：建议紧随 Recovery 之后（推荐链 Recovery → RequestID →
// 其余中间件）——Recovery 必须保持最外层保命，RequestID 在其内侧的代价
// 是 panic 日志拿不到 request_id，属已知取舍（详见 Recovery 的注释）。
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(RequestIDHeader)
			if !serverkit.ValidPropagationValue(rid) {
				rid = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, rid)
			attrs := serverkit.AttrsFromValues(rid, r.Header.Get(UserIDHeader))
			r = r.WithContext(logging.WithAttrs(r.Context(), attrs...))
			next.ServeHTTP(w, r)
		})
	}
}

// RequestIDFrom 返回请求 ctx 中的 request_id，未设置时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	return serverkit.RequestIDFrom(ctx)
}
