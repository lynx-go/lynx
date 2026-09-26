// Package registrytest 提供 Registry / Discovery / Watcher 的契约一致性
// 测试套件（conformance suite）：后端在自己的测试里用工厂接入，契约从
// 注释搬进可执行断言。
//
// 位置约束：套件放在接口旁边（contrib/registry），不依赖 lynxtest——
// design-testkit 约定 lynxtest 留在根模块、零 contrib import。
package registrytest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
)

// ServiceName 是套件使用的服务名：工厂需保证其 Discovery 侧对该名字可用
// （只读后端在工厂内预置数据；可写后端由套件自行 Register）。
const ServiceName = "conformance-svc"

// Instance 返回一条合法的 Passing 实例（服务名固定为 ServiceName；HTTP
// Endpoint 与 Consul 默认 check 类型匹配，便于各后端直接注册）。
func Instance(id string) registry.Instance {
	return registry.Instance{
		Name:      ServiceName,
		ID:        id,
		Version:   "v1",
		Status:    registry.StatusPassing,
		Endpoints: []registry.Endpoint{{Protocol: registry.ProtocolHTTP, Address: "127.0.0.1:8080"}},
	}
}

// BackendFactory 每次调用返回一个全新后端（清理由 t.Cleanup 负责）：
// reg 可为 nil（只读后端，如 DNS），disc 可为 nil。
type BackendFactory func(t *testing.T) (reg registry.Registry, disc registry.Discovery)

// TestRegistry 钉住写接口契约：Close 幂等；Close 后 Register / Deregister /
// Heartbeat 一律返回 registry.ErrClosed。
func TestRegistry(t *testing.T, factory BackendFactory) {
	t.Helper()
	reg, _ := factory(t)
	if reg == nil {
		t.Skip("backend has no Registry side")
	}
	ctx := context.Background()
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := reg.Register(ctx, Instance("i1")); !errors.Is(err, registry.ErrClosed) {
		t.Errorf("Register after Close = %v, want ErrClosed", err)
	}
	if err := reg.Deregister(ctx, ServiceName, "i1"); !errors.Is(err, registry.ErrClosed) {
		t.Errorf("Deregister after Close = %v, want ErrClosed", err)
	}
	if err := reg.Heartbeat(ctx, ServiceName, "i1"); !errors.Is(err, registry.ErrClosed) {
		t.Errorf("Heartbeat after Close = %v, want ErrClosed", err)
	}
}

// TestDiscovery 钉住读接口契约：空名 ErrBadName；GetService 返回快照；
// Close 后读路径返回 registry.ErrClosed（只读后端跳过 close 段）。
func TestDiscovery(t *testing.T, factory BackendFactory) {
	t.Helper()
	reg, disc := factory(t)
	if disc == nil {
		t.Skip("backend has no Discovery side")
	}
	ctx := context.Background()
	if reg != nil {
		if err := reg.Register(ctx, Instance("i1")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	if _, err := disc.GetService(ctx, "", registry.Filter{}); !errors.Is(err, registry.ErrBadName) {
		t.Errorf("GetService(empty name) = %v, want ErrBadName", err)
	}
	if _, err := disc.Watch(ctx, "", registry.Filter{}); !errors.Is(err, registry.ErrBadName) {
		t.Errorf("Watch(empty name) = %v, want ErrBadName", err)
	}

	got, err := disc.GetService(ctx, ServiceName, registry.Filter{})
	if err != nil {
		t.Fatalf("GetService(%q): %v", ServiceName, err)
	}
	if len(got) == 0 {
		t.Fatalf("GetService(%q) returned no instances", ServiceName)
	}

	if reg == nil {
		return
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := disc.GetService(ctx, ServiceName, registry.Filter{}); !errors.Is(err, registry.ErrClosed) {
		t.Errorf("GetService after Close = %v, want ErrClosed", err)
	}
	if _, err := disc.Watch(ctx, ServiceName, registry.Filter{}); !errors.Is(err, registry.ErrClosed) {
		t.Errorf("Watch after Close = %v, want ErrClosed", err)
	}
}

// TestWatcher 钉住 Watcher 契约：首个 Next 立即返回当前快照（含空列表的
// 后端也必须有值可断言时跳过）；Stop 幂等且 Next 返回 ErrWatcherStopped；
// ctx 取消唤醒阻塞中的 Next。
func TestWatcher(t *testing.T, factory BackendFactory) {
	t.Helper()
	reg, disc := factory(t)
	if disc == nil {
		t.Skip("backend has no Discovery side")
	}
	ctx := context.Background()
	if reg != nil {
		if err := reg.Register(ctx, Instance("i1")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	w, err := disc.Watch(ctx, ServiceName, registry.Filter{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	snap, err := nextWithin(t, w)
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if len(snap) == 0 {
		t.Errorf("first Next returned no instances for %q", ServiceName)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, err := w.Next(); !errors.Is(err, registry.ErrWatcherStopped) {
		t.Errorf("Next after Stop = %v, want ErrWatcherStopped", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	w2, err := disc.Watch(cctx, ServiceName, registry.Filter{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if _, err := nextWithin(t, w2); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	cancel()
	if _, err := w2.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("Next after ctx cancel = %v, want context.Canceled", err)
	}
}

// nextWithin 以超时保护执行一次 Next（阻塞语义的失败保护）。
func nextWithin(t *testing.T, w registry.Watcher) ([]registry.Instance, error) {
	t.Helper()
	type result struct {
		insts []registry.Instance
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		insts, err := w.Next()
		ch <- result{insts, err}
	}()
	select {
	case r := <-ch:
		return r.insts, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Next() did not return within timeout")
		return nil, nil
	}
}
