// Package zap 将 zap 高性能日志库包装为 *slog.Logger，
// 提供与框架一致的日志级别与服务标识字段。
package zap

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/samber/lo"
	slogzap "github.com/samber/slog-zap/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// MustNewLogger 创建基于 zap 的 slog 实例，创建失败时 panic。
func MustNewLogger(ctx lynx.AppContext) *slog.Logger {
	return lo.Must1(NewLogger(ctx))
}

// NewLogger 根据应用配置的日志级别创建基于 zap 的 slog 实例，并注入服务标识字段。
func NewLogger(ctx lynx.AppContext) (*slog.Logger, error) {
	_, slogger, err := buildLogger(ctx)
	if err != nil {
		return nil, err
	}
	return slogger, nil
}

// buildLogger 创建 zap 实例与包装后的 slog 实例，并注入服务标识字段。
func buildLogger(ctx lynx.AppContext) (*zap.Logger, *slog.Logger, error) {
	logLevel := lynx.LogLevelFromConfig(ctx.Config())
	if logLevel == "" {
		logLevel = "info"
	}
	zapLogger, err := NewZapLogger(logLevel)
	if err != nil {
		return nil, nil, err
	}
	slogger, err := NewSLogger(zapLogger, logLevel)
	if err != nil {
		return nil, nil, err
	}
	return zapLogger, slogger.With(
		"service_id", lynx.IDFromContext(ctx.Context()),
		"service_name", lynx.NameFromContext(ctx.Context()),
		"version", lynx.VersionFromContext(ctx.Context()),
	), nil
}

// NewZapLogger 创建按生产配置输出的 zap 实例，日志格式为生产配置。
// outputs 指定输出路径（zap 的 OutputPaths），为空时默认输出到 stdout；
// 需要写文件时传入文件路径（替代旧 NewZapLoggerToFile）。
func NewZapLogger(logLevel string, outputs ...string) (*zap.Logger, error) {
	atomicLevel := zap.NewAtomicLevel()

	zapLevel := zap.DebugLevel
	if err := zapLevel.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, err
	}
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	if len(outputs) == 0 {
		outputs = []string{"stdout"}
	}
	zapConfig.OutputPaths = outputs
	return zapConfig.Build()
}

// NewSLogger 将 zap 实例包装为 slog 实例，日志级别按 handler 设置。
func NewSLogger(zlogger *zap.Logger, logLevel string) (*slog.Logger, error) {
	level := slog.LevelDebug
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, err
	}

	// The level is applied per-handler instead of slog.SetLogLoggerLevel to avoid
	// mutating global slog state, which would affect unrelated loggers.
	// zap 侧的级别由 NewZapLogger 的 AtomicLevel 单独控制。
	logger := slog.New(slogzap.Option{Level: level, Logger: zlogger}.NewZapHandler())
	return logger, nil
}

// SyncableLogger wraps slog.Logger with the underlying zap.Logger for Sync capability.
// This allows flushing buffered log entries before application exit.
type SyncableLogger struct {
	*slog.Logger
	zapLogger *zap.Logger
}

// Sync flushes any buffered log entries. Should be called before application exit.
func (l *SyncableLogger) Sync() error {
	return l.zapLogger.Sync()
}

// SyncOnStop 返回一个 OnStop 钩子，在应用关闭前刷新缓冲的 zap 日志，
// 避免进程退出时丢失未落盘的日志记录。
//
//	app.OnStop(zap.SyncOnStop(logger))
func SyncOnStop(l *SyncableLogger) lynx.HookFunc {
	return func(ctx context.Context) error {
		return l.Sync()
	}
}

// NewSyncableLogger creates a SyncableLogger that wraps both slog and zap loggers.
// This allows using slog for structured logging while retaining the ability to Sync.
func NewSyncableLogger(ctx lynx.AppContext) (*SyncableLogger, error) {
	zapLogger, slogger, err := buildLogger(ctx)
	if err != nil {
		return nil, err
	}
	return &SyncableLogger{Logger: slogger, zapLogger: zapLogger}, nil
}
