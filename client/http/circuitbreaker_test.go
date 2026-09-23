package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCircuitBreakerOpensAfterFailures：连续传输层失败达到阈值后熔断
// 打开，后续请求被本地拒绝（ErrCircuitOpen），不再发出网络请求。
func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	c := New(WithCircuitBreaker(CircuitBreakerOptions{
		MinConsecutiveFailures: 2,
		Timeout:                time.Minute, // 不进入 half-open
	}), WithTimeout(time.Second))
	// 不可达地址制造传输层失败（连接拒绝）。
	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), "http://127.0.0.1:1/"); err == nil {
			t.Fatalf("request #%d unexpectedly succeeded", i)
		}
	}
	_, err := c.Get(context.Background(), "http://127.0.0.1:1/")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("third request error = %v, want ErrCircuitOpen", err)
	}
}

// TestCircuitBreakerCountsFailureStatusCodes：FailureStatusCodes 配置的
// 状态码计入失败（响应正常到达但对端不可用）。
func TestCircuitBreakerCountsFailureStatusCodes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(WithCircuitBreaker(CircuitBreakerOptions{
		MinConsecutiveFailures: 2,
		FailureStatusCodes:     []int{http.StatusServiceUnavailable},
		Timeout:                time.Minute,
	}))
	for i := 0; i < 2; i++ {
		resp, err := c.Get(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("request #%d error = %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if _, err := c.Get(context.Background(), srv.URL); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("third request error = %v, want ErrCircuitOpen", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hit %d times, want 2 (third rejected locally)", got)
	}
}

// TestCircuitBreakerExcludesContextCancel：客户端主动取消不计入失败——
// 多次取消后熔断仍 closed，正常请求可通行。
func TestCircuitBreakerExcludesContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	c := New(WithCircuitBreaker(CircuitBreakerOptions{
		MinConsecutiveFailures: 2,
		Timeout:                time.Minute,
	}), WithTimeout(time.Minute))
	// 取消的请求打到真实服务器（连接前的取消）——排除规则生效则不计数。
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = c.Get(ctx, srv.URL)
	}
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("request after cancels error = %v, want success (cancels excluded from counts)", err)
	}
	_ = resp.Body.Close()
}

// TestCircuitBreakerHalfOpenRecovery：open 到期转 half-open 放行探测，
// 探测成功后回到 closed 恢复正常放行。
func TestCircuitBreakerHalfOpenRecovery(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var states []string
	var stMu sync.Mutex
	c := New(WithCircuitBreaker(CircuitBreakerOptions{
		MinConsecutiveFailures: 2,
		FailureStatusCodes:     []int{http.StatusServiceUnavailable},
		Timeout:                50 * time.Millisecond,
		OnStateChange: func(_, from, to string) {
			stMu.Lock()
			states = append(states, from+"->"+to)
			stMu.Unlock()
		},
	}))
	fail.Store(true)
	for i := 0; i < 2; i++ {
		resp, _ := c.Get(context.Background(), srv.URL)
		_ = resp.Body.Close()
	}
	if _, err := c.Get(context.Background(), srv.URL); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("post-failure request error = %v, want ErrCircuitOpen", err)
	}
	// 等 open 到期转 half-open；下游恢复后探测成功 → closed。
	fail.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get(context.Background(), srv.URL)
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("recovery request error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 恢复后再请求不应被拒（closed）。
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("post-recovery request error = %v, want success", err)
	}
	_ = resp.Body.Close()
	stMu.Lock()
	defer stMu.Unlock()
	if len(states) == 0 {
		t.Error("OnStateChange never fired")
	}
}

// TestCircuitBreakerDisabledByDefault：未启用熔断时行为与既有客户端
// 一致（连续失败不产生本地拒绝）。
func TestCircuitBreakerDisabledByDefault(t *testing.T) {
	c := New(WithTimeout(500 * time.Millisecond))
	for i := 0; i < 6; i++ {
		_, err := c.Get(context.Background(), "http://127.0.0.1:1/")
		if err == nil {
			t.Fatalf("request #%d unexpectedly succeeded", i)
		}
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatal("circuit open without WithCircuitBreaker")
		}
	}
}
