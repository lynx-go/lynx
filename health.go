package lynx

import (
	"errors"
	"sync"

	"gocloud.dev/server/health"
)

// HealthChecker 是基于布尔状态的健康检查器，读写均加锁，可并发使用。
type HealthChecker struct {
	healthy bool
	mu      sync.RWMutex
}

// SetHealthy 设置当前健康状态。
func (check *HealthChecker) SetHealthy(healthy bool) {
	check.mu.Lock()
	defer check.mu.Unlock()
	check.healthy = healthy
}

// CheckHealth 实现 health.Checker 接口，当前状态不健康时返回错误。
func (check *HealthChecker) CheckHealth() error {
	check.mu.RLock()
	defer check.mu.RUnlock()
	if !check.healthy {
		return errors.New("unhealthy")
	}
	return nil
}

var _ health.Checker = new(HealthChecker)
