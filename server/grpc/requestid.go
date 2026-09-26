package grpc

import (
	"context"

	"github.com/lynx-go/lynx/internal/serverkit"
)

// RequestIDHeader / UserIDHeader 是 request_id / user_id 的 gRPC metadata
// 键：与 HTTP 头（server/http.RequestIDHeader）及 client/grpc 同源的共享
// wire 键（中性归属 internal/propagation）。业务显式设置 incoming/outgoing
// metadata 时使用本常量，不再硬编码字面量。
const (
	RequestIDHeader = serverkit.RequestIDKey
	UserIDHeader    = serverkit.UserIDKey
)

// RequestIDFrom 返回 RPC ctx 中的 request_id（内置 RequestIDPropagation
// 拦截器从 incoming metadata 解析，见 interceptor 包），未设置时返回
// 空字符串。与 server/http.RequestIDFrom 对称（共享实现见
// internal/serverkit），供业务代码在 handler 内取用（写响应头、落库等）。
func RequestIDFrom(ctx context.Context) string {
	return serverkit.RequestIDFrom(ctx)
}
