package lynx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// drainableService 阻塞运行并实现 Drainer：nilHook 为 true 时 DrainHook
// 返回 nil（不参与排水的服务侧退出）。
type drainableService struct {
	name    string
	record  func(string)
	nilHook bool
}

func (c *drainableService) Name() string                    { return c.name }
func (c *drainableService) Init(ctx AppContext) error       { return nil }
func (c *drainableService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *drainableService) Stop(ctx context.Context) error {
	if c.record != nil {
		c.record("stop:" + c.name)
	}
	return nil
}

func (c *drainableService) DrainHook() HookFunc {
	if c.nilHook {
		return nil
	}
	return func(ctx context.Context) error {
		c.record("drain:" + c.name)
		return nil
	}
}

// TestDrainerHookRunsInDrainPhase：Register 登记时框架自动挂载 Drainer
// 钩子——关停排水边沿执行，先于服务 Stop，多个 Drainer 按注册顺序。
func TestDrainerHookRunsInDrainPhase(t *testing.T) {
	app, err := newLynx(NewOptions(WithDrainTimeout(300 * time.Millisecond)))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	app.Register(&drainableService{name: "first", record: rec.record})
	app.Register(&drainableService{name: "second", record: rec.record})
	// Start 返回 nil 触发正常关停（含排水阶段）。
	app.Register(&failStartService{name: "trigger"})

	if err := app.Run(); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	events := rec.snapshot()
	assertBefore(t, events, "drain:first", "drain:second")
	assertBefore(t, events, "drain:second", "stop:second")
	assertBefore(t, events, "drain:second", "stop:first")
}

// TestDrainerNilHookSkipped：DrainHook 返回 nil 时不挂载，DrainTimeout=0
// 也不触发启动期快失败（依赖 TTL 过期一类服务的退出通道）。
func TestDrainerNilHookSkipped(t *testing.T) {
	app, err := newLynx(NewOptions()) // DrainTimeout=0
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	rec := &eventRecorder{}
	app.Register(&drainableService{name: "ttl-only", record: rec.record, nilHook: true})
	app.Register(&failStartService{name: "trigger"})

	if err := app.Run(); err != nil {
		t.Fatalf("Run() error = %v, want nil (nil DrainHook must not require drain window)", err)
	}
	for _, e := range rec.snapshot() {
		if strings.HasPrefix(e, "drain:") {
			t.Fatalf("nil DrainHook must not run, got event %q", e)
		}
	}
}

// TestDrainFastFailNamesDrainerService：有自动挂载的排水钩子但未启用排水
// 窗口时，Run() 启动期快失败且错误具名来源服务。
func TestDrainFastFailNamesDrainerService(t *testing.T) {
	app, err := newLynx(NewOptions()) // DrainTimeout=0
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	app.Register(&drainableService{name: "registrar", record: func(string) {}})

	err = app.Run()
	if !errors.Is(err, ErrDrainHooksRequireDrainTimeout) {
		t.Fatalf("Run() error = %v, want ErrDrainHooksRequireDrainTimeout", err)
	}
	if !strings.Contains(err.Error(), "registrar") {
		t.Fatalf("Run() error = %v, want service name %q for auto-attached hook", err, "registrar")
	}
}
