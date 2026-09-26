// Package clustertest 提供 cluster.Coordinator / Lease 的契约一致性测试
// 套件（conformance suite）：后端在自己的测试里用工厂接入，契约从注释
// 搬进可执行断言。
//
// 位置约束：套件放在接口旁边（contrib/cluster），不依赖 lynxtest——
// design-testkit 约定 lynxtest 留在根模块、零 contrib import。
package clustertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/cluster"
)

// CoordinatorFactory 每次调用返回一个全新 Coordinator（清理由 t.Cleanup 负责）。
type CoordinatorFactory func(t *testing.T) cluster.Coordinator

// TestCoordinator 钉住协调端口契约：
//   - 入参校验（空名 / 非正 TTL / 已取消 ctx）；
//   - Claim 一次性占位：首次 won、再次 false 且 err=nil；
//   - Acquire 独占：第二次 ok=false 且 err=nil；Release 后 Lease.Context
//     必已取消、重复 Release 幂等、槽位可再次获取；
//   - 声明 TTLAware 的后端：低于 MinTTL 的 Acquire 必须报错（调用方负责
//     经 cluster.MinTTL 钳制）。
func TestCoordinator(t *testing.T, factory CoordinatorFactory) {
	t.Helper()
	ctx := context.Background()
	c := factory(t)

	if _, err := c.Claim(ctx, "", time.Second); !errors.Is(err, cluster.ErrEmptyName) {
		t.Errorf("Claim(empty name) = %v, want ErrEmptyName", err)
	}
	if _, err := c.Claim(ctx, "conformance", 0); !errors.Is(err, cluster.ErrInvalidTTL) {
		t.Errorf("Claim(ttl=0) = %v, want ErrInvalidTTL", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Claim(cctx, "conformance", time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("Claim(cancelled ctx) = %v, want context.Canceled", err)
	}
	if _, _, err := c.Acquire(cctx, "conformance", time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire(cancelled ctx) = %v, want context.Canceled", err)
	}

	// 声明 TTLAware 的后端：调用方负责把 TTL 钳制到下限（cluster.MinTTL）。
	ttl := time.Second
	if min := cluster.MinTTL(c); min > 0 {
		ttl = min
		if _, _, err := c.Acquire(ctx, "conformance-ttl", min-time.Second); err == nil {
			t.Errorf("Acquire below declared MinTTL(%s) = nil, want error", min)
		}
	}

	// Claim：一次性占位。
	if won, err := c.Claim(ctx, "conformance-claim", ttl); err != nil || !won {
		t.Fatalf("first Claim = (%v, %v), want (true, nil)", won, err)
	}
	if won, err := c.Claim(ctx, "conformance-claim", ttl); err != nil || won {
		t.Fatalf("second Claim = (%v, %v), want (false, nil)", won, err)
	}
	lease, ok, err := c.Acquire(ctx, "conformance-lease", ttl)
	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v), want lease", ok, err)
	}
	select {
	case <-lease.Context().Done():
		t.Fatal("Lease.Context cancelled before Release")
	default:
	}
	if _, ok, err := c.Acquire(ctx, "conformance-lease", ttl); err != nil || ok {
		t.Fatalf("second Acquire = (%v, %v), want (false, nil)", ok, err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Lease.Context not cancelled after Release")
	}
	_ = lease.Release(ctx) // 幂等：不 panic（错误允许）

	lease2, ok, err := c.Acquire(ctx, "conformance-lease", ttl)
	if err != nil || !ok {
		t.Fatalf("Acquire after Release = (%v, %v), want lease", ok, err)
	}
	if err := lease2.Release(ctx); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}
