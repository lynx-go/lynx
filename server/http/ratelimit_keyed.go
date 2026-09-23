// 按维度限流（v1.1 承诺回收，ROADMAP G3）：每 key 一个令牌桶，
// key 由调用方从请求提取（路由 / 客户端 IP / 用户标识，预置提取器
// 见 RateLimitKeyPath 等）。桶存储带惰性清扫，防止海量 key（如公网
// IP 维度）无限增长。
package http

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx/logging"
	"golang.org/x/time/rate"
)

// keySweepInterval 是惰性清扫的采样间隔：每处理 keySweepEvery 个请求
// 触发一轮清扫，删除 idleExpiry 内未被使用的桶。无硬性桶数上限：
// 两次清扫间新增 key 的内存上界 = keySweepEvery × 单桶开销（KB 级），
// 公网 IP 维度下完全可接受；确需硬上限时再按需加（YAGNI）。
const (
	keySweepEvery = 1024
	keyIdleExpiry = 10 * time.Minute
)

// keyLimiter 是单个 key 的令牌桶及其最近使用时间（原子更新，读路径
// 无需持锁）。
type keyLimiter struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // unix nano
}

// keyedLimiterStore 维护 key → limiter 的并发安全存储。
type keyedLimiterStore struct {
	mu       sync.RWMutex
	limiters map[string]*keyLimiter
	sweeps   atomic.Int64
}

func newKeyedLimiterStore() *keyedLimiterStore {
	return &keyedLimiterStore{limiters: make(map[string]*keyLimiter)}
}

// allow 对 key 对应的桶执行 Allow；不存在时创建。空 key 同样入表
// （退化为全部空 key 请求共享一个桶，限流不静默失效——如客户端地址
// 解析失败时仍有保护）。
func (s *keyedLimiterStore) allow(key string, rps float64, burst int) bool {
	s.mu.RLock()
	kl := s.limiters[key]
	s.mu.RUnlock()
	if kl == nil {
		s.mu.Lock()
		kl = s.limiters[key]
		if kl == nil {
			kl = &keyLimiter{limiter: rate.NewLimiter(rate.Limit(rps), burst)}
			s.limiters[key] = kl
		}
		s.mu.Unlock()
	}
	kl.lastSeen.Store(time.Now().UnixNano())
	if s.sweeps.Add(1)%keySweepEvery == 0 {
		s.sweep()
	}
	return kl.limiter.Allow()
}

// sweep 删除超过 keyIdleExpiry 未使用的桶。
func (s *keyedLimiterStore) sweep() {
	cutoff := time.Now().Add(-keyIdleExpiry).UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, kl := range s.limiters {
		if kl.lastSeen.Load() < cutoff {
			delete(s.limiters, key)
		}
	}
}

// RateLimitPerKey 返回按维度限流的中间件：key 提取器从请求中取分桶
// 标识（路由路径 / 客户端 IP / 用户标识等，预置提取器见
// RateLimitKeyPath / RateLimitKeyClientIP / RateLimitKeyUserID，也可
// 自定义组合——如 path+userID 拼接实现"单用户单路由"维度）。每个 key
// 独立令牌桶（rps/burst 语义同 RateLimit），互不挤占；桶按惰性清扫
// 回收（见 keySweepEvery 注释）。
//
// rps 必须 > 0，否则构造期 panic（同 RateLimit 的配置错误前置暴露
// 约定）；key 提取器为 nil 时 panic（空指针属于编程错误）。
// 与服务器级 RateLimit 可叠放：先服务器级总量、再维度级分桶（中间件
// 顺序即包裹顺序）。
func RateLimitPerKey(rps float64, key func(*http.Request) string, opts ...RateLimitOption) Middleware {
	if rps <= 0 {
		panic(fmt.Sprintf("http: RateLimitPerKey rps must be > 0, got %v", rps))
	}
	if key == nil {
		panic("http: RateLimitPerKey key extractor must not be nil")
	}
	o := rateLimitOptions{burst: max(1, int(math.Ceil(rps)))}
	for _, opt := range opts {
		opt(&o)
	}
	if o.handler == nil {
		o.handler = defaultRateLimitHandler
	}
	store := newKeyedLimiterStore()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !store.allow(key(r), rps, o.burst) {
				o.handler(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimitKeyPath 按路由路径分桶限流：同一路径的请求共享一个桶，
// 保护慢/贵的端点不被整体流量挤占。
func RateLimitKeyPath(r *http.Request) string { return r.URL.Path }

// RateLimitKeyClientIP 按客户端 IP 分桶限流：取 RemoteAddr 的 host
// 部分。注意：经过反向代理/LB 时 RemoteAddr 是代理地址，需要按真实
// 客户端 IP 限流请基于受信代理写入的头自行编写提取器（信任代理头是
// 部署决策，框架不代为判定）。
func RateLimitKeyClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimitKeyUserID 按用户标识分桶限流：取请求 ctx 日志属性中的
// user_id（与 client 侧传播、logging.WithAttrs 同一来源；未携带时为
// 空串，退化为共享桶）。
func RateLimitKeyUserID(r *http.Request) string {
	for _, a := range logging.AttrsFrom(r.Context()) {
		if a.Key == logging.FieldUserID {
			return a.Value.String()
		}
	}
	return ""
}
