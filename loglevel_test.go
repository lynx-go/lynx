package lynx

import (
	"log/slog"
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
