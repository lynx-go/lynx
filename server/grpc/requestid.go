package grpc

import (
	"context"

	"github.com/lynx-go/lynx/internal/serverkit"
)

// RequestIDFrom 返回 RPC ctx 中的 request_id（内置 RequestIDPropagation
// 拦截器从 incoming metadata 还原，见 interceptor 包），未设置时返回
// 空字符串。与 server/http.RequestIDFrom 对称（共享实现见
// internal/serverkit），供业务代码在 handler 内取用（写响应头、落库等）。
func RequestIDFrom(ctx context.Context) string {
	return serverkit.RequestIDFrom(ctx)
}
