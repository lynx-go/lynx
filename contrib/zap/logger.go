// Package zap 将 zap 高性能日志库包装为 *slog.Logger，
// 提供与框架一致的日志级别与服务标识字段。
package zap

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"syscall"

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
	meta := lynx.Meta(ctx.Context())
	return zapLogger, slogger.With(
		"service.id", meta.ID,
		"service.name", meta.Name,
		"service.version", meta.Version,
	), nil
}

// NewZapLogger 创建按生产配置输出的 zap 实例，日志格式为生产配置。
// outputs 指定输出路径（zap 的 OutputPaths），为空时默认输出到 stdout；
// 需要写文件时传入文件路径（替代旧 NewZapLoggerToFile）。
//
// 级别字符串按框架统一的 slog 域（lynx.ParseLogLevel）校验后再映射到
// zap 级别：此前 zap/slog 两层各自解析，可接受域不一致（"fatal" 仅 zap
// 合法、"info+2" 仅 slog 合法、"warning" 仅 slog 合法），同一字符串在
// 不同层报错，来源不可预期；统一后错误信息一致，且 "warning" 等别名、
// 大小写不敏感解析在两层同时生效。
func NewZapLogger(logLevel string, outputs ...string) (*zap.Logger, error) {
	level, err := lynx.ParseLogLevel(logLevel)
	if err != nil {
		return nil, err
	}

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = zap.NewAtomicLevelAt(zapLevelFromSlog(level))
	// 显式禁用生产默认采样（同级别前 100 条全记、其后每 100 条仅记 1 条）：
	// 高吞吐下 99% 的错误日志会被静默丢弃，而本函数并未暴露采样配置，
	// 用户既不知情也无法关闭。需要采样的场景请自行构建带 sampler 的
	// zap core 传入 NewSLogger。
	zapConfig.Sampling = nil
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	if len(outputs) == 0 {
		outputs = []string{"stdout"}
	}
	zapConfig.OutputPaths = outputs
	return zapConfig.Build()
}

// zapLevelFromSlog 将 slog 标准级别映射到 zap 级别。ParseLogLevel 只产出
// 四个标准级别，映射封闭；不复用 zapcore 的 UnmarshalText 是因为其
// 解析域与 slog 不一致（不接受 "warning" 别名、大小写敏感）。
func zapLevelFromSlog(l slog.Level) zapcore.Level {
	switch l {
	case slog.LevelDebug:
		return zapcore.DebugLevel
	case slog.LevelWarn:
		return zapcore.WarnLevel
	case slog.LevelError:
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// NewSLogger 将 zap 实例包装为 slog 实例，日志级别按 handler 设置。
// 级别字符串与 NewZapLogger 一样按 slog 域（lynx.ParseLogLevel）统一校验。
func NewSLogger(zlogger *zap.Logger, logLevel string) (*slog.Logger, error) {
	level, err := lynx.ParseLogLevel(logLevel)
	if err != nil {
		return nil, err
	}

	// The level is applied per-handler instead of slog.SetLogLoggerLevel to avoid
	// mutating global slog state, which would affect unrelated loggers.
	// zap 侧的级别由 NewZapLogger 的 AtomicLevel 单独控制。
	inner := slogzap.Option{Level: level, Logger: zlogger}.NewZapHandler()
	return slog.New(clampLevelHandler{next: inner}), nil
}

// clampLevelHandler 将非标准 slog 级别（如 slog.LevelError+4）收敛到最近的
// 标准级别后再交给 slog-zap 处理。
//
// 为什么需要：slog-zap v2.7.0 以 map[slog.Level]zapcore.Level 查表映射
// 级别，非标准级别查不到时取 map 零值 zapcore.InfoLevel——比 Error 更
// 严重的自定义级别日志被降级为 Info 写入，且在 zap 侧 AtomicLevel 提高
// 到 Info 之上后被静默丢弃。clamp 方向为向下取最近标准级（>Error 一律
// Error）：不夸大严重度，与 slog 自定义级别"数值越大越严重"的语义一致。
type clampLevelHandler struct {
	next slog.Handler
}

// clampLevel 把任意 slog 级别映射到四个标准级别之一。
func clampLevel(l slog.Level) slog.Level {
	switch {
	case l >= slog.LevelError:
		return slog.LevelError
	case l >= slog.LevelWarn:
		return slog.LevelWarn
	case l >= slog.LevelInfo:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

func (h clampLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, clampLevel(level))
}

func (h clampLevelHandler) Handle(ctx context.Context, r slog.Record) error {
	// Record 按值传入，改写副本的 Level 不影响调用方。
	r.Level = clampLevel(r.Level)
	return h.next.Handle(ctx, r)
}

func (h clampLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return clampLevelHandler{next: h.next.WithAttrs(attrs)}
}

func (h clampLevelHandler) WithGroup(name string) slog.Handler {
	return clampLevelHandler{next: h.next.WithGroup(name)}
}

// SyncableLogger wraps slog.Logger with the underlying zap.Logger for Sync capability.
// This allows flushing buffered log entries before application exit.
type SyncableLogger struct {
	*slog.Logger
	zapLogger *zap.Logger
}

// Sync flushes any buffered log entries. Should be called before application exit.
// 标准流同步时的良性 errno 被忽略：Linux 上 fsync(/dev/stdout) 恒失败
// 返回 EINVAL（zap 已知问题 uber-go/zap#328），macOS 对不可 seek 的
// 标准流返回 EBADF/ENOTTY，只读文件系统返回 EROFS——这些失败由环境
// 固有特性决定，不代表日志丢失，不过滤会让 SyncOnStop 钩子每次关停都
// 误报错误。文件类输出不受影响。
func (l *SyncableLogger) Sync() error {
	err := l.zapLogger.Sync()
	if err == nil {
		return nil
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && isBenignSyncErrno(pathErr.Err) {
		return nil
	}
	return err
}

// isBenignSyncErrno 判定 Sync 报告的 errno 是否为环境固有的良性失败
// （不可 seek 的标准流、只读文件系统），与日志写入路径的健康无关。
// 所列常量在 Linux/macOS/Windows 的 syscall 包均有定义，无需按平台分支。
func isBenignSyncErrno(err error) bool {
	for _, benign := range []error{syscall.EINVAL, syscall.EBADF, syscall.ENOTTY, syscall.EROFS} {
		if errors.Is(err, benign) {
			return true
		}
	}
	return false
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
