package lynx

import (
	"context"
	"testing"
)

// TestAwaitHealthyNonPositiveBudget 锁定"预算 <= 0 时至少检查一次"的
// 既有语义在有界化后仍成立：即使预算已耗尽，也先执行有界检查
// （defaultProbeTimeout 兜底），而不是直接判超时。Windows 粗粒度时钟下
// 同 tick 内 deadline 判定可能不触发，允许在预算边界上多轮询一次，
// 因此断言"至少一次"而非"恰好一次"。
func TestAwaitHealthyNonPositiveBudget(t *testing.T) {
	healthy := &sequenceChecker{}
	if err := awaitHealthy(context.Background(), healthy, 0, readinessPollInterval, nil,
		func(last error) error { return last }); err != nil {
		t.Fatalf("awaitHealthy(healthy) = %v, want nil from a bounded check", err)
	}
	if got := healthy.Calls(); got < 1 {
		t.Errorf("CheckHealth called %d times, want at least 1", got)
	}

	failing := &sequenceChecker{failures: 100}
	if err := awaitHealthy(context.Background(), failing, 0, readinessPollInterval, nil,
		func(last error) error { return last }); err == nil {
		t.Fatal("awaitHealthy(failing) = nil, want timeout error from failed checks")
	}
	if got := failing.Calls(); got < 1 {
		t.Errorf("CheckHealth called %d times, want at least 1", got)
	}
}
