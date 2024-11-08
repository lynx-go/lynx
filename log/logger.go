package log

import (
	"context"
	"log/slog"
)

func Context(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, logger)
}

func FromContext(ctx context.Context) *slog.Logger {
	logger, ok := ctx.Value(keyLogger).(*slog.Logger)
	if ok {
		return logger
	}
	return slog.Default()
}

type ctxLogger struct {
}

var keyLogger = ctxLogger{}
