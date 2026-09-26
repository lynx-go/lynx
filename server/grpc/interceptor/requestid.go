package interceptor

import (
	"context"

	"github.com/lynx-go/lynx/internal/serverkit"
	"github.com/lynx-go/lynx/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// RequestIDPropagation 返回解析并回写请求标识的一元拦截器：从 incoming
// metadata 的 x-request-id/x-user-id 还原日志属性（校验规则与 HTTP 侧一致，
// 见 internal/serverkit.ResolveInbound）——合法 request_id 沿用，缺失/非法
// 生成 UUID；user_id 非法丢弃。解析结果回写响应 metadata（与 HTTP 回写响应
// 头对称，客户端可关联），并经 logging.WithAttrs 注入 RPC ctx——链内所有
// 日志自动携带，handler 发起的下游调用（HTTP/gRPC client）继续透传，与
// server/http 的内置传播共同形成框架两侧的传播闭环（共享键与校验见
// internal/propagation）。
func RequestIDPropagation() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		return handler(resolveInbound(ctx), req)
	}
}

// RequestIDPropagationStream 返回 RequestIDPropagation 的流式版本：
// 解析与回写时机在 handler 拦截链内（首条消息前），流全程生效。
func RequestIDPropagationStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {
		return handler(srv, &ctxStream{ServerStream: ss, ctx: resolveInbound(ss.Context())})
	}
}

// resolveInbound 解析入站请求标识（合法沿用 / request_id 缺失或非法生成
// UUID），回写响应 metadata 并把日志属性注入 ctx。
func resolveInbound(ctx context.Context) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	rid, attrs := serverkit.ResolveInbound(
		firstValue(md, serverkit.RequestIDKey), firstValue(md, serverkit.UserIDKey))
	// 回写响应 metadata（与 HTTP 回写响应头对称）。拦截器先于 handler
	// 执行；无传输流（如直接单测调用拦截器）时 SetHeader 返回错误，不影响
	// 请求处理。
	_ = grpc.SetHeader(ctx, metadata.Pairs(serverkit.RequestIDKey, rid))
	if len(attrs) == 0 {
		return ctx
	}
	return logging.WithAttrs(ctx, attrs...)
}

// ctxStream 以解析后的 ctx 包装 ServerStream，其余行为透传。
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }

// firstValue 取 metadata 多值中的首个，缺失时返回空字符串。
func firstValue(md metadata.MD, key string) string {
	if vs := md.Get(key); len(vs) > 0 {
		return vs[0]
	}
	return ""
}
