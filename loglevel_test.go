package lynx

import (
	"log/slog"
	"os"
	"testing"
)

// fakeLevelConfig 只应答 LogLevelFromConfig 读取的键。
type fakeLevelConfig struct {
	level string
}

func (c fakeLevelConfig) Get(string) any { return nil }
func (c fakeLevelConfig) GetString(key string) string {
	for _, k := range []string{"logging.level", "log-level", "log_level"} {
		if key == k {
			return c.level
		}
	}
	return ""
}
func (c fakeLevelConfig) GetBool(string) bool                { return false }
func (c fakeLevelConfig) GetInt(string) int                  { return 0 }
func (c fakeLevelConfig) GetStringMap(string) map[string]any { return nil }
func (c fakeLevelConfig) GetStringSlice(string) []string     { return nil }
func (c fakeLevelConfig) IsSet(string) bool    { return false }
func (c fakeLevelConfig) Unmarshal(any, ...UnmarshalOption) error { return nil }
func (c fakeLevelConfig) UnmarshalKey(string, any, ...UnmarshalOption) error { return nil }

func newTestApp(t *testing.T) *lynx {
	t.Helper()
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	return app.(*lynx)
}

// TestSetLogLevelDefaultPath：未配置 log-level 时走 SetLogLoggerLevel
// 兜底路径，返回 true 且记账值更新。
func TestSetLogLevelDefaultPath(t *testing.T) {
	defer slog.SetLogLoggerLevel(slog.LevelInfo)
	app := newTestApp(t)
	if app.LogLevel() != slog.LevelInfo {
		t.Errorf("initial LogLevel() = %v, want Info (zero value)", app.LogLevel())
	}
	if ok := app.SetLogLevel(slog.LevelDebug); !ok {
		t.Fatal("SetLogLevel() = false on default path, want true")
	}
	if got := app.LogLevel(); got != slog.LevelDebug {
		t.Errorf("LogLevel() = %v, want Debug", got)
	}
}

// TestSetLogLevelConfiguredPath：配置过 log-level（applyLogLevel 自建
// LevelVar handler）后调整直接修改变量，即时生效。
func TestSetLogLevelConfiguredPath(t *testing.T) {
	app := newTestApp(t)
	app.cfg = fakeLevelConfig{level: "warn"}
	app.applyLogLevel()
	if app.logLevelVar == nil {
		t.Fatal("applyLogLevel() did not install logLevelVar")
	}
	if got := app.LogLevel(); got != slog.LevelWarn {
		t.Errorf("LogLevel() after config = %v, want Warn", got)
	}
	if ok := app.SetLogLevel(slog.LevelError); !ok {
		t.Fatal("SetLogLevel() = false on configured path, want true")
	}
	if got := app.logLevelVar.Level(); got != slog.LevelError {
		t.Errorf("logLevelVar.Level() = %v, want Error (takes effect immediately)", got)
	}
	if got := app.LogLevel(); got != slog.LevelError {
		t.Errorf("LogLevel() = %v, want Error", got)
	}
}

// TestSetLogLevelAfterSetLogger：用户经 WithLoggerProvider/setLogger 定制
// 后框架不再代理级别调整。
func TestSetLogLevelAfterSetLogger(t *testing.T) {
	app := newTestApp(t)
	app.setLogger(slog.Default())
	if ok := app.SetLogLevel(slog.LevelDebug); ok {
		t.Fatal("SetLogLevel() = true after setLogger, want false (customized)")
	}
	if got := app.LogLevel(); got != slog.LevelInfo {
		t.Errorf("LogLevel() = %v, want unchanged Info", got)
	}
}

// writeTempConfig 写一份临时 config.yaml，返回路径。
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLogLevelFlagOverridesConfig：显式 --log-level 经翻译层 Set 进规范键，
// 压过配置文件的 logging.level（运行时 flag 覆盖部署时 config）。
func TestLogLevelFlagOverridesConfig(t *testing.T) {
	cfg := writeTempConfig(t, "logging.level: warn\n")
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-c", cfg, "--log-level", "debug"}

	lp := newIsolated(t, "flag-over-config")
	if got := lp.LogLevel(); got != slog.LevelDebug {
		t.Errorf("LogLevel() = %v, want Debug (flag overrides config)", got)
	}
	if lp.logLevelVar == nil || lp.logLevelVar.Level() != slog.LevelDebug {
		t.Errorf("logLevelVar = %v, want Debug in effect", lp.logLevelVar)
	}
}

// TestLogLevelConfigUsedWithoutFlag：未传 flag 时配置文件生效（翻译层
// 非空守卫不触发，链按序回退），级别键链兼容行为不变。
func TestLogLevelConfigUsedWithoutFlag(t *testing.T) {
	cfg := writeTempConfig(t, "logging.level: warn\n")
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-c", cfg}

	lp := newIsolated(t, "config-level")
	if got := lp.LogLevel(); got != slog.LevelWarn {
		t.Errorf("LogLevel() = %v, want Warn from config", got)
	}
}

// TestLogLevelLegacyFlatKeyStillWorks：兼容回退键 log_level 在无 flag、
// 无规范键时仍生效。
func TestLogLevelLegacyFlatKeyStillWorks(t *testing.T) {
	cfg := writeTempConfig(t, "log_level: warn\n")
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"lynx", "-c", cfg}

	lp := newIsolated(t, "legacy-level")
	if got := lp.LogLevel(); got != slog.LevelWarn {
		t.Errorf("LogLevel() = %v, want Warn from legacy log_level", got)
	}
}

// newIsolated 构造隔离应用并登记清理（测试辅助：统一 newLynx 错误处理）。
func newIsolated(t *testing.T, name string) *lynx {
	t.Helper()
	app, err := newLynx(NewOptions(WithIsolated(), WithName(name)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	t.Cleanup(app.Close)
	return app.(*lynx)
}
