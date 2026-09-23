package serverkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Shutdown 执行有界优雅关停（三个 server 适配器的唯一规则）：
//   - timeout > 0：配置上限与调用方 deadline 取较小者（context 自动继承
//     父 ctx 的更早 deadline）；timeout == 0：无配置上限，仅以调用方为准；
//   - graceful 在独立 goroutine 执行；ctx 结束后先调用 force 解除其阻塞
//     （http.Server.Shutdown 等待连接排空、grpc.Server.GracefulStop 等待
//     在途 RPC 都不会自行返回），再等 graceful 退出，避免与内部清理交错；
//   - graceful 先返回且错误为 context.Canceled / DeadlineExceeded 时同样
//     归类超时并强制关闭；其他错误原样返回；正常返回即成功。
//
// 超时路径统一返回 "<name> graceful shutdown timed out: %w"。
func Shutdown(ctx context.Context, timeout time.Duration, logger *slog.Logger, name string, graceful func(context.Context) error, force func() error) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	done := make(chan error, 1)
	go func() { done <- graceful(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		if isTimeoutErr(err) {
			return forceTimeout(ctx, logger, name, err, force)
		}
		logger.ErrorContext(ctx, "failed to shutdown "+name, "error", err)
		return err
	case <-ctx.Done():
		// 先强制关闭解除 graceful 的阻塞，再等其退出。
		if ferr := force(); ferr != nil {
			logger.ErrorContext(ctx, "failed to force-close "+name, "error", ferr)
		}
		<-done
		logger.ErrorContext(ctx, name+" graceful shutdown timed out, forcing close", "error", ctx.Err())
		return fmt.Errorf("%s graceful shutdown timed out: %w", name, ctx.Err())
	}
}

// isTimeoutErr 判定 graceful 返回的错误是否属于"预算耗尽"路径。
func isTimeoutErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// forceTimeout 处理 graceful 自身因预算耗尽返回的路径：强制关闭活动连接
// 并以统一的超时错误返回。
func forceTimeout(ctx context.Context, logger *slog.Logger, name string, err error, force func() error) error {
	logger.ErrorContext(ctx, name+" graceful shutdown timed out, forcing close", "error", err)
	if ferr := force(); ferr != nil {
		logger.ErrorContext(ctx, "failed to force-close "+name, "error", ferr)
	}
	return fmt.Errorf("%s graceful shutdown timed out: %w", name, err)
}
