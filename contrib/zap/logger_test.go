package zap

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

// fakeApp implements lynx.App minimally for tests.
type fakeApp struct {
	lynx.App
	cfg lynx.Config
}

func (f *fakeApp) Config() lynx.Config      { return f.cfg }
func (f *fakeApp) Context() context.Context { return context.Background() }

func newFakeApp(t *testing.T) *fakeApp {
	t.Helper()
	v := viper.New()
	return &fakeApp{cfg: lynx.NewViperConfig(v)}
}

func (f *fakeApp) set(key, val string) {
	f.cfg.(lynx.ConfigSource).Set(key, val)
}

func TestGetLevelDefaults(t *testing.T) {
	app := newFakeApp(t)
	if got := getLevel(app); got != "info" {
		t.Errorf("getLevel() = %q, want default %q", got, "info")
	}
}

func TestGetLevelKeyPriority(t *testing.T) {
	app := newFakeApp(t)
	app.set("logging.level", "warn")
	if got := getLevel(app); got != "warn" {
		t.Errorf("getLevel() = %q, want %q (logging.level takes priority)", got, "warn")
	}

	app = newFakeApp(t)
	app.set("log_level", "error")
	if got := getLevel(app); got != "error" {
		t.Errorf("getLevel() = %q, want %q", got, "error")
	}

	app = newFakeApp(t)
	app.set("log-level", "debug")
	if got := getLevel(app); got != "debug" {
		t.Errorf("getLevel() = %q, want %q (framework default flag key)", got, "debug")
	}
}

func TestNewZapLogger(t *testing.T) {
	if _, err := NewZapLogger("info"); err != nil {
		t.Errorf("NewZapLogger(info) error = %v", err)
	}
	if _, err := NewZapLogger("bogus"); err == nil {
		t.Error("NewZapLogger(bogus) error = nil, want error")
	}
}

func TestNewZapLoggerToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 无效级别在打开文件之前就返回错误，不会创建文件（回归：级别解析错误
	// 不再被静默忽略）。happy path 由 NewZapLogger 覆盖（相同 Build 逻辑）。
	// 注：zap.Logger 无 Close 方法，Windows 上无法释放文件句柄，
	// 因此文件变体不做文件写入/清理断言。
	if _, err := NewZapLoggerToFile("bogus", path); err == nil {
		t.Error("NewZapLoggerToFile(bogus, ...) error = nil, want error")
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
	app := newFakeApp(t)
	logger, err := NewLogger(app)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if logger == nil {
		t.Fatal("NewLogger() returned nil")
	}
	if logger := MustNewLogger(app); logger == nil {
		t.Fatal("MustNewLogger() returned nil")
	}
}

func TestSyncOnStop(t *testing.T) {
	app := newFakeApp(t)
	l, err := NewSyncableLogger(app)
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
