package zap

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

// fakeEnv implements lynx.Env minimally for tests.
type fakeEnv struct {
	lynx.Env
	cfg lynx.Config
}

func (f *fakeEnv) Config() lynx.Config      { return f.cfg }
func (f *fakeEnv) Context() context.Context { return context.Background() }
func (f *fakeEnv) Logger(...any) *slog.Logger { return slog.Default() }
func (f *fakeEnv) HealthCheckers() []lynx.Checker { return nil }

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	v := viper.New()
	return &fakeEnv{cfg: lynx.NewViperConfig(v)}
}

func (f *fakeEnv) set(key, val string) {
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
//（原 NewZapLoggerToFile 的能力合并进 NewZapLogger）。
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
	env := newFakeEnv(t)
	logger, err := NewLogger(env)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if logger == nil {
		t.Fatal("NewLogger() returned nil")
	}
	if logger := MustNewLogger(env); logger == nil {
		t.Fatal("MustNewLogger() returned nil")
	}
}

func TestSyncOnStop(t *testing.T) {
	env := newFakeEnv(t)
	l, err := NewSyncableLogger(env)
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
	env := newFakeEnv(t)
	env.set("logging.level", "not-a-level")
	if _, err := NewLogger(env); err == nil {
		t.Fatal("expected error for invalid log level")
	}
	if _, err := NewSyncableLogger(env); err == nil {
		t.Fatal("expected error for invalid log level (SyncableLogger)")
	}
}

// TestLogLevelFromConfigKeys 验证级别键来自框架统一解析
//（logging.level 优先，log-level/log_level 为兼容回退），zap 不再
// 维护独立的键优先级实现。
func TestLogLevelFromConfigKeys(t *testing.T) {
	env := newFakeEnv(t)
	env.set("logging.level", "warn")
	env.set("log-level", "error")
	env.set("log_level", "debug")
	if got := lynx.LogLevelFromConfig(env.Config()); got != "warn" {
		t.Errorf("LogLevelFromConfig() = %q, want warn", got)
	}

	env = newFakeEnv(t)
	env.set("log_level", "debug")
	if got := lynx.LogLevelFromConfig(env.Config()); got != "debug" {
		t.Errorf("LogLevelFromConfig() = %q, want debug", got)
	}

	env = newFakeEnv(t)
	if got := lynx.LogLevelFromConfig(env.Config()); got != "" {
		t.Errorf("LogLevelFromConfig() = %q, want empty", got)
	}
}
