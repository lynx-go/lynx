package interceptor

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/internal/serverkit"
	"github.com/lynx-go/lynx/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// RequestIDPropagation 返回把 incoming metadata 中的 x-request-id/x-user-id
// 还原为日志属性的一元拦截器：lynx grpc client（client/grpc）的传播
// 拦截器把 ctx 日志属性写入 outgoing metadata，本拦截器在服务端对称还原，
// 经 logging.WithAttrs 注入 RPC ctx——链内所有日志自动携带，handler 发起
// 的下游调用（HTTP/gRPC client）继续透传，与 server/http 的内置传播
// 共同形成框架两侧的传播闭环（共享键与校验见 internal/serverkit）。
//
// 校验规则与 HTTP 侧一致（长度 ≤128、字符集 [A-Za-z0-9-_]）：非法值
// 丢弃；缺失的字段不注入。注入为 logging.WithAttrs 覆盖语义（与 HTTP
// 侧一致）；gRPC 服务端每 RPC 新建 ctx，不存在既有属性冲突。
func RequestIDPropagation() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if attrs := attrsFromMetadata(ctx); len(attrs) > 0 {
			ctx = logging.WithAttrs(ctx, attrs...)
		}
		return handler(ctx, req)
	}
}

// RequestIDPropagationStream 返回 RequestIDPropagation 的流式版本：
// 还原时机在 handler 拦截链内（首条消息前），流全程生效。
func RequestIDPropagationStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {
		attrs := attrsFromMetadata(ss.Context())
		if len(attrs) == 0 {
			return handler(srv, ss)
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: logging.WithAttrs(ss.Context(), attrs...)})
	}
}

// ctxStream 以还原后的 ctx 包装 ServerStream，其余行为透传。
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }

// attrsFromMetadata 从 incoming metadata 提取合法的传播属性：
// 键为共享 wire 键（x-request-id / x-user-id）；多值取首个。
func attrsFromMetadata(ctx context.Context) []slog.Attr {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	return serverkit.AttrsFromValues(firstValue(md, serverkit.RequestIDKey), firstValue(md, serverkit.UserIDKey))
}

// firstValue 取 metadata 多值中的首个，缺失时返回空字符串。
func firstValue(md metadata.MD, key string) string {
	if vs := md.Get(key); len(vs) > 0 {
		return vs[0]
	}
	return ""
}
