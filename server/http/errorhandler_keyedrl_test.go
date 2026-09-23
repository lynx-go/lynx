package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx/logging"
)

// TestServerErrorHandlerFallback：Server.NewErrorHandler 的 nil 兜底
// 依次取 WithErrorHandler 的服务器级默认 → 包级 DefaultErrorHandler。
func TestServerErrorHandlerFallback(t *testing.T) {
	mux := http.NewServeMux()
	srv := NewServer(mux)
	// 未配置 WithErrorHandler：兜底为包级默认（返回 500 JSON 体）。
	h := srv.NewErrorHandler(nil, func(_ context.Context, _ http.ResponseWriter, _ *http.Request) error {
		return errBoom
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("no server handler: status = %d, want 500 (package default)", rec.Code)
	}

	custom := func(_ context.Context, w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusTeapot)
	}
	srv2 := NewServer(mux, WithErrorHandler(custom))
	h2 := srv2.NewErrorHandler(nil, func(_ context.Context, _ http.ResponseWriter, _ *http.Request) error {
		return errBoom
	})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusTeapot {
		t.Errorf("server handler: status = %d, want 418 (server-level default)", rec2.Code)
	}

	// 显式 h 优先于服务器级默认。
	h3 := srv2.NewErrorHandler(func(_ context.Context, w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusGone)
	}, func(_ context.Context, _ http.ResponseWriter, _ *http.Request) error {
		return errBoom
	})
	rec3 := httptest.NewRecorder()
	h3.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec3.Code != http.StatusGone {
		t.Errorf("explicit handler: status = %d, want 410 (explicit wins)", rec3.Code)
	}
}

// keyedLimitEnv 构造按维度限流的测试环境：返回放行计数器与中间件
// 包装后的 handler。
func keyedLimitEnv(t *testing.T, rps float64, key func(*http.Request) string, burst int) (http.Handler, *counter) {
	t.Helper()
	var allowed counter
	var opts []RateLimitOption
	if burst > 0 {
		opts = append(opts, WithBurst(burst))
	}
	mw := RateLimitPerKey(rps, key, opts...)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		allowed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	return h, &allowed
}

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) Add(d int) {
	c.mu.Lock()
	c.n += d
	c.mu.Unlock()
}

func (c *counter) Load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

var errBoom = boomError{}

type boomError struct{}

func (boomError) Error() string { return "boom" }

// TestRateLimitPerKeyIsolatesBuckets：不同 key 的桶互不挤占。
func TestRateLimitPerKeyIsolatesBuckets(t *testing.T) {
	h, allowed := keyedLimitEnv(t, 1, RateLimitKeyClientIP, 1)
	// 同一 IP 连打两次：burst=1，第二次被拒。
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if got := allowed.Load(); got != 1 {
		t.Errorf("same-key allowed = %d, want 1 (bucket exhausted)", got)
	}
	// 不同 IP（伪造 RemoteAddr）：各自有独立桶，均放行。
	for _, addr := range []string{"10.0.0.1:1234", "10.0.0.2:1234"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	if got := allowed.Load(); got != 3 {
		t.Errorf("cross-key allowed = %d, want 3 (independent buckets)", got)
	}
}

// TestRateLimitPerKeyPath：按路由分桶——不同路径互不挤占。
func TestRateLimitPerKeyPath(t *testing.T) {
	h, allowed := keyedLimitEnv(t, 1, RateLimitKeyPath, 1)
	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/expensive", nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/cheap", nil))
	if got := allowed.Load(); got != 2 {
		t.Errorf("allowed = %d, want 2 (/expensive exhausted, /cheap independent)", got)
	}
}

// TestRateLimitPerKeyUserID：按 user_id 日志属性分桶，未携带时共享
// 空串桶。
func TestRateLimitPerKeyUserID(t *testing.T) {
	h, allowed := keyedLimitEnv(t, 1, RateLimitKeyUserID, 1)
	reqA := httptest.NewRequest(http.MethodGet, "/", nil)
	reqA = reqA.WithContext(logging.WithAttrs(reqA.Context(), slog.String(logging.FieldUserID, "user-a")))
	reqB := httptest.NewRequest(http.MethodGet, "/", nil)
	reqB = reqB.WithContext(logging.WithAttrs(reqB.Context(), slog.String(logging.FieldUserID, "user-b")))
	h.ServeHTTP(httptest.NewRecorder(), reqA)
	h.ServeHTTP(httptest.NewRecorder(), reqA) // user-a 桶耗尽
	h.ServeHTTP(httptest.NewRecorder(), reqB) // user-b 独立
	if got := allowed.Load(); got != 2 {
		t.Errorf("allowed = %d, want 2 (per-user buckets)", got)
	}
}

// TestRateLimitPerKeySweep：空闲桶被惰性清扫回收。
func TestRateLimitPerKeySweep(t *testing.T) {
	store := newKeyedLimiterStore()
	store.allow("k1", 1, 1)
	// 人为把 k1 的 lastSeen 回拨到过期边界之外。
	store.mu.Lock()
	store.limiters["k1"].lastSeen.Store(time.Now().Add(-2 * keyIdleExpiry).UnixNano())
	store.mu.Unlock()
	store.sweep()
	store.mu.RLock()
	n := len(store.limiters)
	store.mu.RUnlock()
	if n != 0 {
		t.Errorf("after sweep = %d limiters, want 0 (idle entry removed)", n)
	}
}

// TestRateLimitPerKeyPanics：非法配置构造期 panic。
func TestRateLimitPerKeyPanics(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic", name)
			}
		}()
		fn()
	}
	mustPanic("rps<=0", func() { RateLimitPerKey(0, RateLimitKeyPath) })
	mustPanic("nil key", func() { RateLimitPerKey(1, nil) })
}
