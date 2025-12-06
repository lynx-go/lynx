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

func MustNewLogger(app lynx.Lynx) *slog.Logger {
	return lo.Must1(NewLogger(app))
}

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

func NewZapLogger(logLevel string) (*zap.Logger, error) {
	level := slog.LevelDebug
	atomicLevel := zap.NewAtomicLevel()

	zapLevel := zap.DebugLevel
	_ = level.UnmarshalText([]byte(logLevel))
	_ = zapLevel.UnmarshalText([]byte(logLevel))
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return zapConfig.Build()
}

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

	slog.SetLogLoggerLevel(level)
	logger := slog.New(slogzap.Option{Level: level, Logger: zlogger}.NewZapHandler())
	return logger, nil
}
