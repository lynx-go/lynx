package zap

import (
	"log/slog"

	"github.com/lynx-go/lynx"
	slogzap "github.com/samber/slog-zap/v2"
	"go.uber.org/zap"
)

func NewLogger(app lynx.Lynx, logLevel string) *slog.Logger {
	level := slog.LevelDebug
	atomicLevel := zap.NewAtomicLevel()
	if logLevel == "" {
		logLevel = "debug"
	}

	zapLevel := zap.DebugLevel
	_ = level.UnmarshalText([]byte(logLevel))
	_ = zapLevel.UnmarshalText([]byte(logLevel))
	atomicLevel.SetLevel(zapLevel)

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = atomicLevel
	//zapConfig.EncoderConfig.EncodeTime= zap.En
	zapLogger, _ := zapConfig.Build()
	slog.SetLogLoggerLevel(level)
	logger := slog.New(slogzap.Option{Level: level, Logger: zapLogger}.NewZapHandler())
	logger = logger.With("version", app.Option().Version, "service_name", app.Option().Name, "service_id", app.Option().ID)
	return logger
}
