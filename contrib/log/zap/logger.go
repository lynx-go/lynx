package zap

import (
	"log/slog"

	"github.com/lynx-go/lynx"
	slogzap "github.com/samber/slog-zap/v2"
	"go.uber.org/zap"
)

func NewLogger(lx lynx.Lynx, logLevel string) *slog.Logger {
	level := slog.LevelDebug
	atomicLevel := zap.NewAtomicLevel()
	if logLevel == "" {
		logLevel = lx.Option().LogLevel
	}

	zapLevel := zap.DebugLevel
	if logLevel != "" {
		_ = level.UnmarshalText([]byte(logLevel))
		_ = zapLevel.UnmarshalText([]byte(logLevel))
	}
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	//zapConfig.EncoderConfig.EncodeTime= zap.En
	zapLogger, _ := zapConfig.Build()
	slog.SetLogLoggerLevel(level)
	logger := slog.New(slogzap.Option{Level: level, Logger: zapLogger}.NewZapHandler())
	logger = logger.With("version", lx.Option().Version, "service_name", lx.Option().Name, "service_id", lx.Option().ID)
	return logger
}
