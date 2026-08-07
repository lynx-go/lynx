package zap

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

// fakeCtx implements lynx.AppContext minimally for tests.
type fakeCtx struct {
	cfg lynx.Config
}

func (f *fakeCtx) Config() lynx.Config            { return f.cfg }
func (f *fakeCtx) Context() context.Context       { return context.Background() }
func (f *fakeCtx) Logger(...any) *slog.Logger     { return slog.Default() }
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
