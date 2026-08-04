package zap

import (
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/samber/lo"
	slogzap "github.com/samber/slog-zap/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func getLevel(app lynx.Lynx) string {
	logLevel := app.Config().GetString("logging.level")
	if logLevel == "" {
		logLevel = app.Config().GetString("log_level")
	}
	if logLevel == "" {
		logLevel = "debug"
	}
	return logLevel
}

// MustNewLogger 创建基于 zap 的 slog 实例，创建失败时 panic。
func MustNewLogger(app lynx.Lynx) *slog.Logger {
	return lo.Must1(NewLogger(app))
}

// NewLogger 根据应用配置的日志级别创建基于 zap 的 slog 实例，并注入服务标识字段。
func NewLogger(app lynx.Lynx) (*slog.Logger, error) {
	logLevel := getLevel(app)
	zapLogger, err := NewZapLogger(logLevel)
	if err != nil {
		return nil, err
	}
	slogger, err := NewSLogger(zapLogger, logLevel)
	if err != nil {
		return nil, err
	}

	return slogger.With("service_id", lynx.IDFromContext(app.Context()), "service_name", lynx.NameFromContext(app.Context()), "version", lynx.VersionFromContext(app.Context())), nil
}

// NewZapLoggerToFile 创建输出到指定文件的 zap 实例，日志格式为生产配置。
func NewZapLoggerToFile(logLevel string, logFile string) (*zap.Logger, error) {
	atomicLevel := zap.NewAtomicLevel()

	zapLevel := zap.DebugLevel
	if err := zapLevel.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, err
	}
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapConfig.OutputPaths = []string{logFile}
	return zapConfig.Build()
}

// NewZapLogger 创建按生产配置输出的 zap 实例。
func NewZapLogger(logLevel string) (*zap.Logger, error) {
	atomicLevel := zap.NewAtomicLevel()

	zapLevel := zap.DebugLevel
	if err := zapLevel.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, err
	}
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return zapConfig.Build()
}

// NewSLogger 将 zap 实例包装为 slog 实例，日志级别按 handler 设置。
func NewSLogger(zlogger *zap.Logger, logLevel string) (*slog.Logger, error) {
	level := slog.LevelDebug
	atomicLevel := zap.NewAtomicLevel()

	zapLevel := zap.DebugLevel
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, err
	}

	if err := zapLevel.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, err
	}
	atomicLevel.SetLevel(zapLevel)

	// The level is applied per-handler instead of slog.SetLogLoggerLevel to avoid
	// mutating global slog state, which would affect unrelated loggers.
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

// NewSyncableLogger creates a SyncableLogger that wraps both slog and zap loggers.
// This allows using slog for structured logging while retaining the ability to Sync.
func NewSyncableLogger(app lynx.Lynx) (*SyncableLogger, error) {
	logLevel := getLevel(app)
	zapLogger, err := NewZapLogger(logLevel)
	if err != nil {
		return nil, err
	}
	slogger, err := NewSLogger(zapLogger, logLevel)
	if err != nil {
		return nil, err
	}
	return &SyncableLogger{
		Logger:    slogger.With("service_id", lynx.IDFromContext(app.Context()), "service_name", lynx.NameFromContext(app.Context()), "version", lynx.VersionFromContext(app.Context())),
		zapLogger: zapLogger,
	}, nil
}
