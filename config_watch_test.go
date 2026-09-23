package lynx

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// pollFor 轮询 cond 至真或超时。
func pollFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestConfigWatchRequiresFile：显式要求热更新但配置无文件来源时，
// startConfigWatch 快失败。
func TestConfigWatchRequiresFile(t *testing.T) {
	app := newTestApp(t)
	app.o.ConfigWatch = true
	if err := app.startConfigWatch(); err == nil {
		t.Fatal("startConfigWatch() error = nil, want error without config file")
	}
}

// TestConfigWatchReloadPublishesEvent：文件变更经 viper 自动重读后，
// 框架向总线发布 lynx.config.updated，且后续读取返回新值。
func TestConfigWatchReloadPublishesEvent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte("k: v1\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	app := newTestApp(t)
	app.c = viper.New()
	app.c.SetConfigFile(file)
	if err := app.c.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}
	if err := app.startConfigWatch(); err != nil {
		t.Fatalf("startConfigWatch() error = %v", err)
	}

	var got atomic.Int32
	err := eventbus.ConfigUpdatedTopic.Subscribe(context.Background(),
		func(_ context.Context, e *eventbus.Event[eventbus.ConfigUpdatedEvent]) error {
			if e.Payload.File == file {
				got.Add(1)
			}
			return nil
		}, eventbus.WithHandlerName("watch-test"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := os.WriteFile(file, []byte("k: v2\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	// fsnotify 事件异步到达：轮询等待回调与 viper 重读完成。
	// fsnotify 事件异步到达，且同一次写文件在不同平台可能触发多个事件
	//（如 Write+Chmod）：收到至少一次即认为桥接生效。
	if !pollFor(t, 5*time.Second, func() bool { return got.Load() >= 1 }) {
		t.Fatalf("config updated event not received within 5s (got %d)", got.Load())
	}
	// 事件到达时 viper 已重读：后续 Get 返回新值。
	if v := app.c.GetString("k"); v != "v2" {
		t.Errorf("GetString(k) = %q after reload, want v2", v)
	}
}
