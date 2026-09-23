// 出站熔断（ROADMAP G2）：经典三态（closed/open/half-open）熔断器，
// 基于 sony/gobreaker/v2 封装——本包公共 API 零 gobreaker 类型
//（实现可替换，kratos v3 的同款教训），配置经 CircuitBreakerOptions
// 映射，集成于 Client.Do 的最外层（重试之外：一次调用整体成败只计
// 一次，熔断打开时不再进入重试循环）。
package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	gobreaker "github.com/sony/gobreaker/v2"
)

// ErrCircuitOpen 表示熔断器处于 open 态（或 half-open 探测超限），
// 请求被本地拒绝——未发出任何网络请求。业务可用 errors.Is 判定并
// 走降级路径。
var ErrCircuitOpen = errors.New("http client: circuit breaker open")

// CircuitBreakerOptions 是出站熔断的配置项（零 gobreaker 类型，
// 字段语义见各注释；缺省值见 newCircuitBreaker）。
type CircuitBreakerOptions struct {
	// Name 标识熔断器（状态回调与日志中的 breaker 名），缺省
	// "http-client"。
	Name string
	// MaxRequests 是 half-open 态放行的探测请求数；0 表示只放行 1 个
	//（gobreaker 语义）。
	MaxRequests uint32
	// Interval 是 closed 态计数的周期清零间隔（固定窗口）；0 表示不
	// 周期清零。
	Interval time.Duration
	// Timeout 是 open 态时长，到期转 half-open 放行探测；<=0 默认 60s。
	Timeout time.Duration
	// MinConsecutiveFailures 是连续失败多少次后打开熔断；0 默认 5。
	// 客户端主动取消（ctx 取消/超时）不计入失败（见 newCircuitBreaker
	// 的排除规则）。
	MinConsecutiveFailures uint32
	// FailureStatusCodes 额外计入失败的响应状态码。缺省仅传输层错误
	//（err != nil）算失败——HTTP 4xx/5xx 是对端的正常响应语义；运维
	// 认定"对端 503 = 下游不可用"时按需加入。
	FailureStatusCodes []int
	// OnStateChange 在状态迁移时回调（from/to 取 "closed"/"open"/
	// "half-open"）。框架侧同时记录 Info 日志；需要桥接 eventbus 的
	// 应用在此回调里自行 Publish。
	OnStateChange func(name, from, to string)
}

// WithCircuitBreaker 启用出站熔断：连续失败达到阈值后打开熔断，
// 后续请求本地快速拒绝（ErrCircuitOpen，不发网络请求）；open 态到期
// 转 half-open 放行探测请求，探测成功回到 closed。与 WithRetry 并存时
// 熔断在重试之外层——重试的全部尝试构成一次调用的整体成败。
func WithCircuitBreaker(cfg CircuitBreakerOptions) Option {
	return func(o *Options) {
		o.CircuitBreaker = &cfg
	}
}

// newCircuitBreaker 按配置映射 gobreaker 实例（实现细节，勿外泄类型）：
//   - 失败判定：传输层错误（err != nil）；客户端取消/超时被 IsExcluded
//     排除——那是调用方的决定，不是下游故障，计入会误开熔断；
//   - 触发条件：连续失败 >= MinConsecutiveFailures（缺省 5）；
//   - 状态迁移记 Info 日志，并转交 OnStateChange 回调。
func newCircuitBreaker(cfg CircuitBreakerOptions, logger *slog.Logger) *gobreaker.TwoStepCircuitBreaker[struct{}] {
	minFailures := cfg.MinConsecutiveFailures
	if minFailures == 0 {
		minFailures = 5
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	name := cfg.Name
	if name == "" {
		name = "http-client"
	}
	st := gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     timeout,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return c.ConsecutiveFailures >= minFailures
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Info("http client: circuit breaker state changed",
				"breaker", name, "from", from.String(), "to", to.String())
			if cfg.OnStateChange != nil {
				cfg.OnStateChange(name, from.String(), to.String())
			}
		},
		IsSuccessful: func(err error) bool { return err == nil },
		IsExcluded: func(err error) bool {
			return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		},
	}
	return gobreaker.NewTwoStepCircuitBreaker[struct{}](st)
}

// doBounded 经熔断（若启用）发送：Allow 判定放行，do 返回（响应头
// 到达）即上报成败——响应体读取失败不计入，熔断以"请求是否送达并
// 获得响应"为准（与 kratos 熔断中间件同语义）。
func (c *Client) doBounded(req *http.Request) (*http.Response, error) {
	if c.o.CircuitBreaker == nil {
		return c.do(req)
	}
	done, allowErr := c.cb.Allow()
	if allowErr != nil {
		return nil, fmt.Errorf("%w: %s", ErrCircuitOpen, allowErr)
	}
	resp, err := c.do(req)
	done(c.reportFailure(resp, err))
	return resp, err
}

// reportFailure 合成上报给熔断的错误：仅当配置把该状态码计入失败时，
// 把"成功到达的失败状态响应"转写为 error（合成错误仅用于熔断计数，
// 不改变返回给调用方的 resp/err）。
func (c *Client) reportFailure(resp *http.Response, err error) error {
	if err != nil {
		return err
	}
	if resp == nil || len(c.o.CircuitBreaker.FailureStatusCodes) == 0 {
		return nil
	}
	for _, code := range c.o.CircuitBreaker.FailureStatusCodes {
		if resp.StatusCode == code {
			return fmt.Errorf("http client: status %d counted as failure", resp.StatusCode)
		}
	}
	return nil
}
