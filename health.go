package lynx

import (
	"errors"
	"sync/atomic"
)

// Checker 是健康检查接口：返回 nil 表示健康，返回非 nil 表示不健康。
// 替代 gocloud.dev/server/health.Checker，消除对该模块的依赖。
type Checker interface {
	CheckHealth() error
}

// HealthChecker 是基于布尔状态的健康检查器，可并发使用。
type HealthChecker struct {
	healthy atomic.Bool
}

// SetHealthy 设置当前健康状态。
func (check *HealthChecker) SetHealthy(healthy bool) {
	check.healthy.Store(healthy)
}

// CheckHealth 实现 Checker 接口，当前状态不健康时返回错误。
func (check *HealthChecker) CheckHealth() error {
	if !check.healthy.Load() {
		return errors.New("unhealthy")
	}
	return nil
}

var _ Checker = new(HealthChecker)
