package lynx

import (
	"sync"
	"testing"
)

func TestHealthChecker_SetHealthy(t *testing.T) {
	checker := &HealthChecker{}

	t.Run("set healthy to true", func(t *testing.T) {
		checker.SetHealthy(true)
		if err := checker.CheckHealth(); err != nil {
			t.Errorf("expected healthy, got error: %v", err)
		}
	})

	t.Run("set healthy to false", func(t *testing.T) {
		checker.SetHealthy(false)
		if err := checker.CheckHealth(); err == nil {
			t.Error("expected unhealthy, got nil error")
		}
	})
}

func TestHealthChecker_CheckHealth(t *testing.T) {
	t.Run("concurrent access", func(t *testing.T) {
		checker := &HealthChecker{}
		var wg sync.WaitGroup

		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					checker.SetHealthy(i%4 == 0)
				} else {
					checker.CheckHealth()
				}
			}(i)
		}

		wg.Wait()
		checker.SetHealthy(true)
		if err := checker.CheckHealth(); err != nil {
			t.Errorf("expected healthy after concurrent access, got error: %v", err)
		}
	})

	t.Run("unhealthy returns error", func(t *testing.T) {
		checker := &HealthChecker{healthy: false}
		err := checker.CheckHealth()

		if err == nil {
			t.Error("expected error when unhealthy, got nil")
		}

		if err.Error() != "unhealthy" {
			t.Errorf("expected 'unhealthy' error message, got %v", err)
		}
	})

	t.Run("healthy returns nil", func(t *testing.T) {
		checker := &HealthChecker{healthy: true}
		err := checker.CheckHealth()

		if err != nil {
			t.Errorf("expected nil error when healthy, got %v", err)
		}
	})
}

func TestHealthChecker_ThreadSafety(t *testing.T) {
	checker := &HealthChecker{}
	var wg sync.WaitGroup

	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				checker.CheckHealth()
			}
		}
	}()

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			checker.SetHealthy(i%2 == 0)
		}(i)
	}

	wg.Wait()
	close(done)

	checker.SetHealthy(true)
	if err := checker.CheckHealth(); err != nil {
		t.Errorf("expected healthy after thread safety test, got error: %v", err)
	}
}

func TestNewHealthChecker(t *testing.T) {
	checker := &HealthChecker{}

	if err := checker.CheckHealth(); err == nil {
		t.Error("expected unhealthy by default, got nil")
	}

	checker.SetHealthy(true)
	if err := checker.CheckHealth(); err != nil {
		t.Errorf("expected healthy after setting, got error: %v", err)
	}
}
