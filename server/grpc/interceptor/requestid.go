package interceptor

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// maxPropagationValueLength 是入站传播值的最大长度：与 server/http 侧
// SC-22 同一动机——metadata 是客户端可控输入，超长或含非法字符的值
// 通常是异常客户端/攻击载荷，直接丢弃，不注入日志（避免刷日志与
// 污染下游）。
const maxPropagationValueLength = 128

// RequestIDPropagation 返回把 incoming metadata 中的 request_id/user_id
// 还原为日志属性的一元拦截器：lynx grpc client（client/grpc）的传播
// 拦截器把 ctx 日志属性写入 outgoing metadata（key 与日志字段同名），
// 本拦截器在服务端对称还原，经 logging.WithAttrs 注入 RPC ctx——链内
// 所有日志自动携带，handler 发起的下游调用（HTTP/gRPC client）继续
// 透传，与 server/http.WithRequestID 共同形成框架两侧的传播闭环。
//
// 校验规则与 HTTP 侧一致（长度 ≤128、字符集 [A-Za-z0-9-_]）：非法值
// 丢弃；缺失的字段不注入。注入为 logging.WithAttrs 覆盖语义（与
// HTTP 侧一致）；gRPC 服务端每 RPC 新建 ctx，不存在既有属性冲突。
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
// key 与日志字段同名（metadata 规范小写，字段名已满足）；多值取首个。
func attrsFromMetadata(ctx context.Context) []slog.Attr {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	var attrs []slog.Attr
	for _, field := range []string{logging.FieldRequestID, logging.FieldUserID} {
		values := md.Get(field)
		if len(values) == 0 || !validPropagationValue(values[0]) {
			continue
		}
		attrs = append(attrs, slog.String(field, values[0]))
	}
	return attrs
}

// validPropagationValue 判定入站传播值是否可安全注入日志：非空、
// 长度 ≤128、字符集限定 [A-Za-z0-9-_]（与 server/http 侧
// validRequestID 同一规则；两包各自私有实现，避免跨 server 包的
// 公共依赖面）。
func validPropagationValue(v string) bool {
	if v == "" || len(v) > maxPropagationValueLength {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
