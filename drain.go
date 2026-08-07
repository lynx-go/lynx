package lynx

import (
	"errors"
	"sync/atomic"
)

// drainChecker 是框架内部的排水检查器（不导出）：关停流程进入排水窗口时
// 置位 draining，使 readiness 聚合（app.HealthCheckers()）立即失败，
// 让负载均衡器在真实关停前完成摘流。仅当 Options.DrainTimeout > 0 时由
// newLynx 注册进 healthCheckers；DrainTimeout=0（默认）时不注册，
// HealthCheckers() 快照内容与 v1.0 完全一致。
type drainChecker struct {
	draining atomic.Bool
}

// SetDraining 设置排水状态。
func (d *drainChecker) SetDraining(draining bool) {
	d.draining.Store(draining)
}

// CheckHealth 实现 Checker：排水期间返回错误，其余返回 nil。
func (d *drainChecker) CheckHealth() error {
	if d.draining.Load() {
		return errors.New("draining")
	}
	return nil
}

var _ Checker = (*drainChecker)(nil)
