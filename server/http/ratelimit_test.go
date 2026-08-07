package http

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRateLimitBurstThenReject：burst 内全过、超出后稳定 429（缺省 JSON
// 错误体）。
func TestRateLimitBurstThenReject(t *testing.T) {
	handler := RateLimit(1, WithBurst(3))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("request %d error = %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d (within burst)", i, resp.StatusCode, http.StatusOK)
		}
	}

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("request %d error = %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("request %d status = %d, want %d (over limit)", i, resp.StatusCode, http.StatusTooManyRequests)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("request %d Content-Type = %q, want application/json", i, ct)
		}
		if got := strings.TrimSpace(string(body)); got != `{"error":{"message":"rate limit exceeded"}}` {
			t.Errorf("request %d body = %q, want rate limit JSON body", i, got)
		}
	}
}

// TestRateLimitDefaultBurst：缺省 burst = max(1, rps)。
func TestRateLimitDefaultBurst(t *testing.T) {
	handler := RateLimit(5)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for i := 0; i < 5; i++ {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("request %d error = %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d (default burst)", i, resp.StatusCode, http.StatusOK)
		}
	}

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("request over limit error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d (over default burst)", resp.StatusCode, http.StatusTooManyRequests)
	}
}

// TestRateLimitCustomHandler：WithRateLimitHandler 覆盖缺省 429 响应。
func TestRateLimitCustomHandler(t *testing.T) {
	handler := RateLimit(1, WithBurst(1), WithRateLimitHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimited", "true")
		http.Error(w, "slow down", http.StatusServiceUnavailable)
	}))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	if resp, err := http.Get(srv.URL + "/"); err == nil {
		_ = resp.Body.Close()
	} else {
		t.Fatalf("first request error = %v", err)
	}

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("second request error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if resp.Header.Get("X-RateLimited") != "true" {
		t.Error("X-RateLimited header not set by custom handler")
	}
	if !strings.Contains(string(body), "slow down") {
		t.Errorf("body = %q, want custom rate limit body", string(body))
	}
}

// TestRateLimitZeroRPSPanics：rps ≤ 0 构造期 panic，报错信息明确。
func TestRateLimitZeroRPSPanics(t *testing.T) {
	for _, rps := range []float64{0, -1} {
		func() {
			defer func() {
				p := recover()
				if p == nil {
					t.Errorf("RateLimit(%v) did not panic", rps)
					return
				}
				msg := fmt.Sprint(p)
				if !strings.Contains(msg, "rps must be > 0") || !strings.Contains(msg, "got "+fmt.Sprint(rps)) {
					t.Errorf("RateLimit(%v) panic = %q, want explicit rps error", rps, msg)
				}
			}()
			RateLimit(rps)
		}()
	}
}

// TestRateLimitConcurrent：-race 下并发打满无竞态；放行/拒绝总数守恒。
func TestRateLimitConcurrent(t *testing.T) {
	handler := RateLimit(1000, WithBurst(10))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	const n = 200
	var wg sync.WaitGroup
	var allowed, rejected atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/")
			if err != nil {
				t.Errorf("GET error = %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				allowed.Add(1)
			case http.StatusTooManyRequests:
				rejected.Add(1)
			default:
				t.Errorf("unexpected status = %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	total := allowed.Load() + rejected.Load()
	if total != n {
		t.Errorf("allowed+rejected = %d, want %d", total, n)
	}
	if allowed.Load() == 0 {
		t.Error("no request allowed, limiter broken")
	}
	if rejected.Load() == 0 {
		t.Error("no request rejected, limiter not limiting")
	}
}
