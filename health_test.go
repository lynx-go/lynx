package lynx

import (
	"sync"
	"testing"
)

func TestHealthChecker(t *testing.T) {
	checker := &HealthChecker{}
	if err := checker.CheckHealth(); err == nil {
		t.Error("CheckHealth() should fail when unhealthy")
	}

	checker.SetHealthy(true)
	if err := checker.CheckHealth(); err != nil {
		t.Errorf("CheckHealth() error = %v, want nil when healthy", err)
	}

	checker.SetHealthy(false)
	if err := checker.CheckHealth(); err == nil {
		t.Error("CheckHealth() should fail after SetHealthy(false)")
	}
}

func TestHealthCheckerConcurrent(t *testing.T) {
	checker := &HealthChecker{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				checker.SetHealthy(j%2 == 0)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = checker.CheckHealth()
			}
		}()
	}
	wg.Wait()
}
