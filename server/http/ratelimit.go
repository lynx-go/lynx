package http

import (
	"errors"
	"fmt"
	"math"
	"net/http"

	"golang.org/x/time/rate"
)

// RateLimitOption 用于配置 RateLimit 中间件的选项函数。
type RateLimitOption func(*rateLimitOptions)

type rateLimitOptions struct {
	burst   int
	handler http.HandlerFunc
}

// WithBurst 设置令牌桶的突发容量，缺省为 max(1, rps)（rps 非整数时向上
// 取整）。突发容量决定瞬时允许的并发放行数：rps 相同时 burst 越大，短时
// 突发越宽松，但长期平均速率仍受 rps 约束。
func WithBurst(n int) RateLimitOption {
	return func(o *rateLimitOptions) {
		o.burst = n
	}
}

// WithRateLimitHandler 设置超限请求的处理函数，缺省写 429 +
// {"error":{"message":"rate limit exceeded"}}（application/json）。
func WithRateLimitHandler(fn http.HandlerFunc) RateLimitOption {
	return func(o *rateLimitOptions) {
		o.handler = fn
	}
}

// defaultRateLimitHandler 是缺省限流响应：429 + 统一 JSON 错误体。
func defaultRateLimitHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusTooManyRequests, errors.New("rate limit exceeded"))
}

// RateLimit 返回服务器级令牌桶限流中间件：rps 为每秒放行请求数，中间件
// 实例持有一个共享 limiter，超出限速的请求由限流 handler 处理（缺省
// 429 + JSON 错误体）。rps 必须 > 0，否则构造期直接 panic（配置错误应当
// 在启动阶段暴露，而非运行期静默放行/拒绝全部请求）。
//
// v1.1 只提供服务器级全局限流（全部请求共享同一 limiter）；按路由、按
// IP/用户维度限流定位 v1.2。
//
// 建议与 Recovery 中间件搭配：Recovery 声明在最外层（WithMiddleware 的
// 第一个参数），RateLimit 随后——限流 handler 抛 panic 时同样能被恢复。
func RateLimit(rps float64, opts ...RateLimitOption) Middleware {
	if rps <= 0 {
		panic(fmt.Sprintf("http: RateLimit rps must be > 0, got %v", rps))
	}
	o := rateLimitOptions{burst: max(1, int(math.Ceil(rps)))}
	for _, opt := range opts {
		opt(&o)
	}
	if o.handler == nil {
		o.handler = defaultRateLimitHandler
	}
	limiter := rate.NewLimiter(rate.Limit(rps), o.burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow() {
				o.handler(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
