package zap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// fakeCtx implements lynx.AppContext minimally for tests.
type fakeCtx struct {
	cfg lynx.Config
}

func (f *fakeCtx) Config() lynx.Config            { return f.cfg }
func (f *fakeCtx) Context() context.Context       { return context.Background() }
func (f *fakeCtx) Logger(...any) *slog.Logger     { return slog.Default() }
func (f *fakeCtx) Bus() eventbus.Bus              { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (f *fakeCtx) HealthCheckers() []lynx.Checker { return nil }
func (f *fakeCtx) Close()                         {}

func newFakeCtx(t *testing.T) *fakeCtx {
	t.Helper()
	v := viper.New()
	return &fakeCtx{cfg: lynx.NewViperConfig(v)}
}

func (f *fakeCtx) set(key, val string) {
	f.cfg.(lynx.ConfigSource).Set(key, val)
}

func TestNewZapLogger(t *testing.T) {
	if _, err := NewZapLogger("info"); err != nil {
		t.Errorf("NewZapLogger(info) error = %v", err)
	}
	if _, err := NewZapLogger("bogus"); err == nil {
		t.Error("NewZapLogger(bogus) error = nil, want error")
	}
}

// TestNewZapLoggerToFileViaOutputs 验证文件输出经 outputs 参数实现
// （原 NewZapLoggerToFile 的能力合并进 NewZapLogger）。
func TestNewZapLoggerToFileViaOutputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 无效级别在打开文件之前就返回错误，不会创建文件（回归：级别解析错误
	// 不再被静默忽略）。happy path 由 TestNewZapLogger 覆盖（相同 Build 逻辑）。
	// 注：zap.Logger 无 Close 方法，Windows 上无法释放文件句柄，
	// 因此文件变体不做文件写入/清理断言。
	if _, err := NewZapLogger("bogus", path); err == nil {
		t.Error("NewZapLogger(bogus, path) error = nil, want error")
	}
}

func TestNewSLogger(t *testing.T) {
	zl, err := NewZapLogger("info")
	if err != nil {
		t.Fatalf("NewZapLogger() error = %v", err)
	}
	if _, err := NewSLogger(zl, "info"); err != nil {
		t.Errorf("NewSLogger(info) error = %v", err)
	}
	if _, err := NewSLogger(zl, "bogus"); err == nil {
		t.Error("NewSLogger(bogus) error = nil, want error")
	}
}

func TestNewLoggerAndMustNewLogger(t *testing.T) {
	ctx := newFakeCtx(t)
	logger, err := NewLogger(ctx)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if logger == nil {
		t.Fatal("NewLogger() returned nil")
	}
	if logger := MustNewLogger(ctx); logger == nil {
		t.Fatal("MustNewLogger() returned nil")
	}
}

func TestSyncOnStop(t *testing.T) {
	ctx := newFakeCtx(t)
	l, err := NewSyncableLogger(ctx)
	if err != nil {
		t.Fatalf("NewSyncableLogger() error = %v", err)
	}
	hook := SyncOnStop(l)
	if hook == nil {
		t.Fatal("SyncOnStop() returned nil")
	}
	if err := hook(context.Background()); err != nil {
		t.Errorf("SyncOnStop() error = %v", err)
	}
}

// TestNewLoggerInvalidLevelError 回归：非法日志级别配置下 NewLogger
// 必须返回错误而非静默回退。
func TestNewLoggerInvalidLevelError(t *testing.T) {
	ctx := newFakeCtx(t)
	ctx.set("logging.level", "not-a-level")
	if _, err := NewLogger(ctx); err == nil {
		t.Fatal("expected error for invalid log level")
	}
	if _, err := NewSyncableLogger(ctx); err == nil {
		t.Fatal("expected error for invalid log level (SyncableLogger)")
	}
}

// TestNewZapLoggerLevelDomainUnified 回归 AUX-11：级别解析统一走 slog 域
// （lynx.ParseLogLevel）。此前两层各自解析导致可接受域不一致——"fatal"
// 仅 zap 层合法（slog 层随后报错，来源不可预期），"warning"/大写形式仅
// slog 层合法（zap 层直接报错）。
func TestNewZapLoggerLevelDomainUnified(t *testing.T) {
	if _, err := NewZapLogger("warning"); err != nil {
		t.Errorf("NewZapLogger(warning) error = %v, want nil (slog 别名应两层同时生效)", err)
	}
	if _, err := NewZapLogger("INFO"); err != nil {
		t.Errorf("NewZapLogger(INFO) error = %v, want nil (大小写不敏感)", err)
	}
	if _, err := NewZapLogger("fatal"); err == nil {
		t.Error("NewZapLogger(fatal) error = nil, want error (slog 域不接受 fatal)")
	}
	if _, err := NewSLogger(zap.NewNop(), "info+2"); err == nil {
		t.Error("NewSLogger(info+2) error = nil, want error (slog 域不接受偏移形式)")
	}
}

// newBufferZapLogger 构建写入内存 buffer 的 zap 实例，供内容断言测试
// 使用（NewZapLogger 只接受文件路径，buffer 需直接构建 core）。
func newBufferZapLogger(buf *bytes.Buffer) *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zap.DebugLevel)
	return zap.New(core)
}

// TestSLoggerLevelMappingAndFiltering 覆盖 AUX-17 测试缺口：级别映射与
// 过滤此前零内容断言。标准四级保持原映射、handler 级别过滤生效。
func TestSLoggerLevelMappingAndFiltering(t *testing.T) {
	var buf bytes.Buffer
	// handler 级别取 debug 以放行全部四级，映射断言才可见。
	slogger, err := NewSLogger(newBufferZapLogger(&buf), "debug")
	if err != nil {
		t.Fatalf("NewSLogger: %v", err)
	}
	slogger.Debug("dbg-msg")
	slogger.Info("info-msg")
	slogger.Warn("warn-msg")
	slogger.Error("error-msg")

	out := buf.String()
	for _, want := range []string{
		`"level":"debug"`, `"msg":"dbg-msg"`,
		`"level":"info"`, `"msg":"info-msg"`,
		`"level":"warn"`, `"msg":"warn-msg"`,
		`"level":"error"`, `"msg":"error-msg"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s, got: %s", want, out)
		}
	}

	// handler 级别（slog 域）过滤：warn 级实例的 info 日志不落盘。
	buf.Reset()
	warnLogger, err := NewSLogger(newBufferZapLogger(&buf), "warn")
	if err != nil {
		t.Fatalf("NewSLogger(warn): %v", err)
	}
	warnLogger.Info("should-not-appear")
	warnLogger.Warn("should-appear")
	out = buf.String()
	if strings.Contains(out, "should-not-appear") {
		t.Errorf("info entry passed warn-level handler, got: %s", out)
	}
	if !strings.Contains(out, "should-appear") {
		t.Errorf("warn entry missing, got: %s", out)
	}
}

// TestSLoggerClampsNonStandardLevels 回归 AUX-04：slog-zap 以 map 查表
// 映射级别，非标准级别（如 slog.LevelError+4）查不到时取零值 InfoLevel，
// 被降级为 Info 写入、在 zap 级别提高后被静默丢弃；包装层必须把 >Error
// 的级别 clamp 到 Error 落盘，同时保持标准四级映射不变。
func TestSLoggerClampsNonStandardLevels(t *testing.T) {
	var buf bytes.Buffer
	slogger, err := NewSLogger(newBufferZapLogger(&buf), "info")
	if err != nil {
		t.Fatalf("NewSLogger: %v", err)
	}
	// slog.LevelError+4 与 slog.LevelError+1：>Error 一律 clamp 到 Error。
	slogger.Log(context.Background(), slog.Level(12), "level-12-msg")
	slogger.Log(context.Background(), slog.LevelError+1, "level-error-plus1-msg")
	// 区间内向下取最近标准级（Info 与 Warn 之间 → Info）。
	slogger.Log(context.Background(), slog.Level(2), "level-2-msg")

	out := buf.String()
	for _, msg := range []string{"level-12-msg", "level-error-plus1-msg"} {
		idx := strings.Index(out, msg)
		if idx < 0 {
			t.Fatalf("output missing %q, got: %s", msg, out)
		}
		// 断言该条目所在行以 error 级别写入（非 Info 降级）。
		lineStart := strings.LastIndex(out[:idx], "\n") + 1
		line := out[lineStart:]
		if !strings.Contains(line[:len(line)-len(msg)], `"level":"error"`) {
			t.Errorf("entry %q not written at error level, got: %s", msg, line)
		}
	}
	if !strings.Contains(out, `"level-2-msg"`) {
		t.Errorf("in-range level entry missing, got: %s", out)
	}
}

// TestNewZapLoggerDisablesSampling 回归 AUX-01：生产默认采样会让同级别
// 前 100 条全记、其后每 100 条仅记 1 条（250 条只留 ~102 条），高吞吐下
// 错误日志静默丢失；必须禁用采样，>200 条 Error 全部落盘。
func TestNewZapLoggerDisablesSampling(t *testing.T) {
	dir, err := os.MkdirTemp("", "lynx-zap-aux01-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	// Windows 下 zap 持有文件句柄且无 Close API，RemoveAll 可能失败；
	// 清理失败不影响断言，忽略错误（残留目录交由系统临时目录策略回收）。
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "app.log")

	zl, err := NewZapLogger("info", path)
	if err != nil {
		t.Fatalf("NewZapLogger: %v", err)
	}
	const total = 250
	const msg = "aux01-sampling-regression"
	for i := 0; i < total; i++ {
		zl.Error(msg, zap.Int("i", i))
	}
	if err := zl.Sync(); err != nil {
		// 标准流类良性 errno 不影响内容断言（文件输出正常不会出现，
		// 防御性放行见 isBenignSyncErrno）。
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || !isBenignSyncErrno(pathErr.Err) {
			t.Fatalf("Sync: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// 按消息计数而非按行数：Error 级可能附带多行 stacktrace。
	if got := strings.Count(string(data), msg); got != total {
		t.Errorf("logged %d entries, want %d (sampling must be disabled; default sampler would keep ~102)", got, total)
	}
}

// fakeSyncCore 是最小 zapcore.Core 替身：仅用于让 Sync 返回指定错误。
type fakeSyncCore struct {
	err error
}

func (c *fakeSyncCore) Enabled(zapcore.Level) bool    { return false }
func (c *fakeSyncCore) With([]zap.Field) zapcore.Core { return c }
func (c *fakeSyncCore) Check(zapcore.Entry, *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return nil
}
func (c *fakeSyncCore) Write(zapcore.Entry, []zap.Field) error { return nil }
func (c *fakeSyncCore) Sync() error {
	return c.err
}

// TestSyncIgnoresBenignErrnos 回归 AUX-12：EINVAL/EBADF/ENOTTY/EROFS 是
// 标准流与只读文件系统的固有失败，Sync 必须放行；其他错误照常上抛。
func TestSyncIgnoresBenignErrnos(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.EBADF, syscall.ENOTTY, syscall.EROFS} {
		l := &SyncableLogger{zapLogger: zap.New(&fakeSyncCore{err: &os.PathError{Op: "sync", Path: "/dev/stdout", Err: errno}})}
		if err := l.Sync(); err != nil {
			t.Errorf("Sync() with benign errno %v = %v, want nil", errno, err)
		}
	}

	l := &SyncableLogger{zapLogger: zap.New(&fakeSyncCore{err: &os.PathError{Op: "sync", Path: "/data/app.log", Err: syscall.EIO}})}
	if err := l.Sync(); err == nil {
		t.Error("Sync() with EIO = nil, want error")
	}
	l = &SyncableLogger{zapLogger: zap.New(&fakeSyncCore{err: errors.New("disk on fire")})}
	if err := l.Sync(); err == nil {
		t.Error("Sync() with non-PathError = nil, want error")
	}
}

// TestLogLevelFromConfigKeys 验证级别键来自框架统一解析
// （logging.level 优先，log-level/log_level 为兼容回退），zap 不再
// 维护独立的键优先级实现。
func TestLogLevelFromConfigKeys(t *testing.T) {
	ctx := newFakeCtx(t)
	ctx.set("logging.level", "warn")
	ctx.set("log-level", "error")
	ctx.set("log_level", "debug")
	if got := lynx.LogLevelFromConfig(ctx.Config()); got != "warn" {
		t.Errorf("LogLevelFromConfig() = %q, want warn", got)
	}

	ctx = newFakeCtx(t)
	ctx.set("log_level", "debug")
	if got := lynx.LogLevelFromConfig(ctx.Config()); got != "debug" {
		t.Errorf("LogLevelFromConfig() = %q, want debug", got)
	}

	ctx = newFakeCtx(t)
	if got := lynx.LogLevelFromConfig(ctx.Config()); got != "" {
		t.Errorf("LogLevelFromConfig() = %q, want empty", got)
	}
}
