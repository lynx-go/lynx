package interceptor

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Logging returns a unary server interceptor that logs each request
func Logging(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		logger.InfoContext(ctx, "gRPC request", "method", info.FullMethod)

		resp, err := handler(ctx, req)
		if err != nil {
			logger.ErrorContext(ctx, "gRPC request failed", "method", info.FullMethod, "duration", time.Since(start), "error", err)
		} else {
			logger.InfoContext(ctx, "gRPC request completed", "method", info.FullMethod, "duration", time.Since(start))
		}

		return resp, err
	}
}

// Recovery returns a unary server interceptor that recovers from panics
func Recovery() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Errorf(codes.Internal, "panic recovered: %v", r)
			}
		}()
		return handler(ctx, req)
	}
}
