// Package interceptor 提供 gRPC 服务端拦截器：日志与 panic 恢复
// （一元与流式各一套）。
package interceptor

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Logging returns a unary server interceptor that logs each request
// at Info level. 与 HTTP 侧默认关闭请求日志不同，gRPC 侧历史行为是
// 无条件每 RPC 记日志——保持缺省不变（兼容），可经 server 侧
// WithRequestLog(false) 关闭、WithRequestLogLevel 调级。
func Logging(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return LoggingWithLevel(logger, slog.LevelInfo)
}

// LoggingWithLevel 返回按指定级别记录请求日志的一元拦截器：生产环境
// 可降噪到 Debug（配合 server 侧 WithRequestLogLevel），行为同 Logging。
func LoggingWithLevel(logger *slog.Logger, level slog.Level) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		logger.Log(ctx, level, "gRPC request", "method", info.FullMethod)

		resp, err := handler(ctx, req)
		if err != nil {
			logger.Log(ctx, level, "gRPC request failed", "method", info.FullMethod, "duration", time.Since(start), "error", err)
		} else {
			logger.Log(ctx, level, "gRPC request completed", "method", info.FullMethod, "duration", time.Since(start))
		}

		return resp, err
	}
}

// Recovery returns a unary server interceptor that recovers from panics:
// panic 值与完整调用栈记入日志（缺省 slog.Default，可用
// RecoveryWithLogger 注入实例），对客户端只返回通用 internal error——
// panic 值可能携带连接串/路径/内部状态等细节，不得回传（信息泄露）。
func Recovery() grpc.UnaryServerInterceptor {
	return RecoveryWithLogger(slog.Default())
}

// RecoveryWithLogger 返回携带注入日志实例的 panic 恢复一元拦截器：
// 恢复时记录 panic 值 + debug.Stack()（此前完全无本地痕迹，SC-06），
// 并返回通用 "internal error"（SC-04）。logger 为 nil 时回退
// slog.Default()。
func RecoveryWithLogger(logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				// 详情只进日志：method 定位、panic 值与完整调用栈
				//（SC-06）；客户端仅收到通用错误（SC-04）。
				logger.ErrorContext(ctx, "gRPC handler panic recovered",
					"method", info.FullMethod,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// LoggingStream returns a stream server interceptor that logs each RPC.
func LoggingStream(logger *slog.Logger) grpc.StreamServerInterceptor {
	return LoggingStreamWithLevel(logger, slog.LevelInfo)
}

// LoggingStreamWithLevel 返回按指定级别记录日志的流式拦截器，
// 行为同 LoggingStream（见 LoggingWithLevel）。
func LoggingStreamWithLevel(logger *slog.Logger, level slog.Level) grpc.StreamServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		ctx := ss.Context()
		logger.Log(ctx, level, "gRPC stream request", "method", info.FullMethod)

		err := handler(srv, ss)
		if err != nil {
			logger.Log(ctx, level, "gRPC stream request failed", "method", info.FullMethod, "duration", time.Since(start), "error", err)
		} else {
			logger.Log(ctx, level, "gRPC stream request completed", "method", info.FullMethod, "duration", time.Since(start))
		}
		return err
	}
}

// RecoveryStream returns a stream server interceptor that recovers from
// panics：同 Recovery——日志 + 通用错误（SC-04/SC-06）。
func RecoveryStream() grpc.StreamServerInterceptor {
	return RecoveryStreamWithLogger(slog.Default())
}

// RecoveryStreamWithLogger 返回携带注入日志实例的 panic 恢复流式
// 拦截器，行为同 RecoveryWithLogger。
func RecoveryStreamWithLogger(logger *slog.Logger) grpc.StreamServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ss.Context(), "gRPC stream handler panic recovered",
					"method", info.FullMethod,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}
