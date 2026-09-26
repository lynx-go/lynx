package http

import (
	"context"
	"net/http"

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
// 注册顺序：默认装配中本中间件包在用户中间件（WithMiddleware）外侧——
// 用户挂的 Recovery 位于其内侧，panic 日志能拿到 request_id；若手动装配
// 并希望 Recovery 拿到最外层保命位置，把本中间件放在 Recovery 内侧即可
// （代价是 Recovery 的 panic 日志不再带 request_id，详见 Recovery 注释）。
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid, attrs := serverkit.ResolveInbound(r.Header.Get(RequestIDHeader), r.Header.Get(UserIDHeader))
			w.Header().Set(RequestIDHeader, rid)
			r = r.WithContext(logging.WithAttrs(r.Context(), attrs...))
			next.ServeHTTP(w, r)
		})
	}
}

// RequestIDFrom 返回请求 ctx 中的 request_id，未设置时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	return serverkit.RequestIDFrom(ctx)
}
