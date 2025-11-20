package zap

import (
	"log/slog"

	"github.com/lynx-go/lynx"
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

func NewLogger(app lynx.Lynx) *slog.Logger {
	level := slog.LevelDebug
	atomicLevel := zap.NewAtomicLevel()
	logLevel := getLevel(app)

	zapLevel := zap.DebugLevel
	_ = level.UnmarshalText([]byte(logLevel))
	_ = zapLevel.UnmarshalText([]byte(logLevel))
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapLogger, _ := zapConfig.Build()
	slog.SetLogLoggerLevel(level)
	logger := slog.New(slogzap.Option{Level: level, Logger: zapLogger}.NewZapHandler())
	return logger.With("service_id", lynx.IDFromContext(app.Context()), "service_name", lynx.NameFromContext(app.Context()), "version", lynx.VersionFromContext(app.Context()))
}

func NewZapLogger(zlogger *zap.Logger, logLevel string) *slog.Logger {
	level := slog.LevelDebug
	atomicLevel := zap.NewAtomicLevel()

	zapLevel := zap.DebugLevel
	_ = level.UnmarshalText([]byte(logLevel))
	_ = zapLevel.UnmarshalText([]byte(logLevel))
	atomicLevel.SetLevel(zapLevel)

	slog.SetLogLoggerLevel(level)
	logger := slog.New(slogzap.Option{Level: level, Logger: zlogger}.NewZapHandler())
	return logger
}
