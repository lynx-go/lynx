package lynx

import (
	"errors"
	"gocloud.dev/server/health"
	"sync"
)

type HealthChecker struct {
	healthy bool
	mu      sync.RWMutex
}

func (check *HealthChecker) SetHealthy(healthy bool) {
	check.mu.Lock()
	defer check.mu.Unlock()
	check.healthy = healthy
}

func (check *HealthChecker) CheckHealth() error {
	check.mu.RLock()
	defer check.mu.RUnlock()
	if !check.healthy {
		return errors.New("unhealthy")
	}
	return nil
}

var _ health.Checker = new(HealthChecker)
