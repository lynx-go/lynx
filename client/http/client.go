// Package http 提供框架的 HTTP 客户端：OpenTelemetry 插装、trace 与
// 日志属性（request_id/user_id）传播、整体超时与可配置重试。
// 与 server/http 相对，面向服务间调用场景。
package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/lynx-go/lynx/logging"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader 是 request_id 透传的 HTTP 请求头名，与服务端
// server/http.RequestIDHeader 同值（两处各自定义同名常量，保持
// client 不反向依赖 server 包；语义靠常量名与注释维系一致）。
const RequestIDHeader = "X-Request-Id"

// UserIDHeader 是 user_id 透传的 HTTP 请求头名。
const UserIDHeader = "X-User-Id"

// DefaultTimeout 是整体超时的缺省值（30s）。
const DefaultTimeout = 30 * time.Second

// Options 是 HTTP 客户端的配置项。
type Options struct {
	// Timeout 为整体超时，0 表示无超时。
	Timeout time.Duration
	// TracerProvider 为插装使用的 TracerProvider，nil 时用全局（缺省 noop）。
	TracerProvider trace.TracerProvider
	// Propagator 为传播上下文注入使用的 TextMapPropagator，nil 时用全局。
	Propagator propagation.TextMapPropagator
	// Logger 为重试等内部日志使用的实例，nil 时用 slog.Default()。
	Logger *slog.Logger
	// Retry 非 nil 时启用重试（WithRetry 装配）。
	Retry *RetryOptions
	// ClientOptions 透传配置底层 *http.Client 的逃生口。
	ClientOptions func(*http.Client)
}

// Option 用于配置 HTTP 客户端 Options 的选项函数。
type Option func(*Options)

// WithTimeout 设置整体超时：自 Do 发起起覆盖全部尝试（含重试），
// Do 返回后仍约束响应体读取（ctx 到期后读取返回错误）。缺省 30s；
// 调用方 ctx 已带 deadline 时不叠加，以调用方为准。传 0 表示无超时。
func WithTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.Timeout = d
	}
}

// WithTracerProvider 设置客户端插装使用的 TracerProvider；nil 时使用
// 全局（缺省 noop）provider。provider 生命周期由调用方负责，
// 显式注入、不修改进程全局。
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

// WithPropagator 设置向请求注入传播上下文（traceparent 等）使用的
// TextMapPropagator；nil 时使用全局 propagator。
func WithPropagator(p propagation.TextMapPropagator) Option {
	return func(o *Options) {
		o.Propagator = p
	}
}

// WithLogger 设置客户端内部日志（如不可重试原因）使用的日志实例；
// nil 时使用 slog.Default()。
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithRetry 启用重试：maxAttempts 为总尝试次数（含首次），
// maxAttempts < 1 时 panic。重试条件与 Retry-After 语义见 Client.Do。
// 退避使用指数退避（cenkalti/backoff），opts 可调整退避参数。
func WithRetry(maxAttempts int, opts ...RetryOption) Option {
	if maxAttempts < 1 {
		panic("http client: WithRetry maxAttempts must be >= 1")
	}
	ro := RetryOptions{
		MaxAttempts:     maxAttempts,
		InitialInterval: backoff.DefaultInitialInterval,
		MaxInterval:     backoff.DefaultMaxInterval,
	}
	for _, opt := range opts {
		opt(&ro)
	}
	return func(o *Options) {
		o.Retry = &ro
	}
}

// RetryOption 用于配置重试策略的选项函数。
type RetryOption func(*RetryOptions)

// RetryOptions 是重试策略的配置项。
type RetryOptions struct {
	// MaxAttempts 为总尝试次数（含首次）。
	MaxAttempts int
	// InitialInterval 为首次重试等待的基准时长（指数退避起点）。
	InitialInterval time.Duration
	// MaxInterval 为退避等待时长的上限。
	MaxInterval time.Duration
}

// WithRetryInitialInterval 设置退避起点，缺省 500ms。
func WithRetryInitialInterval(d time.Duration) RetryOption {
	return func(o *RetryOptions) {
		o.InitialInterval = d
	}
}

// WithRetryMaxInterval 设置退避等待时长上限，缺省 60s。
func WithRetryMaxInterval(d time.Duration) RetryOption {
	return func(o *RetryOptions) {
		o.MaxInterval = d
	}
}

// WithClientOptions 透传配置底层 *http.Client 的逃生口：在内部默认
// transport（otel 插装的 DefaultTransport 浅克隆）装配之后应用，
// 可整体替换 Transport 或覆盖其他字段。
func WithClientOptions(fn func(*http.Client)) Option {
	return func(o *Options) {
		o.ClientOptions = fn
	}
}

// Client 是框架的 HTTP 客户端，实现 lynx 的传播与超时/重试约定。
type Client struct {
	client *http.Client
	o      Options
}

// New 创建 HTTP 客户端，零配置可用：整体超时 30s、不重试；
// transport 为 http.DefaultTransport 的浅克隆 + otelhttp 插装
// （不修改进程全局默认值）。
func New(opts ...Option) *Client {
	options := Options{Timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&options)
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	base := http.DefaultTransport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		base = t.Clone()
	}
	otelOpts := []otelhttp.Option{}
	if options.TracerProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithTracerProvider(options.TracerProvider))
	}
	if options.Propagator != nil {
		otelOpts = append(otelOpts, otelhttp.WithPropagators(options.Propagator))
	}
	hc := &http.Client{Transport: otelhttp.NewTransport(base, otelOpts...)}
	if options.ClientOptions != nil {
		options.ClientOptions(hc)
	}
	return &Client{client: hc, o: options}
}

// Do 发送请求并返回响应，行为约定如下：
//
//   - 传播：将 req.Context() 中的日志属性写入请求头（request_id →
//     X-Request-Id、user_id → X-User-Id），已存在的同名请求头不覆盖；
//     otelhttp 插装同时注入 traceparent 等传播上下文。对端配合
//     server/http.WithRequestID 即可还原 request_id，形成传播闭环。
//   - 超时：整体超时（见 WithTimeout）覆盖全部重试；调用方 ctx 已带
//     deadline 时以调用方为准。
//   - 重试：仅当 WithRetry 启用时生效。重试条件为传输层错误（调用方
//     ctx 取消/超时除外）或状态码 429/502/503/504；429/503 响应携带
//     Retry-After 头时，等待至少其指示的时长（秒数或 HTTP-date）。
//     带请求体（req.Body 非 nil）且不可重放（req.GetBody 为 nil）的
//     请求只发送一次、不重试，并记 debug 日志。每次重试前会自动关闭
//     上一次尝试的响应体；可重放请求在重试前经 GetBody 重建请求体。
//
// 本方法不读取、不关闭响应体：调用方负责读取并关闭（重试中途丢弃的
// 响应体由客户端内部关闭）。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c.o.Timeout > 0 {
		if _, ok := req.Context().Deadline(); !ok {
			ctx, cancel := context.WithTimeout(req.Context(), c.o.Timeout)
			defer cancel()
			req = req.WithContext(ctx)
		}
	}
	propagateAttrs(req)
	if c.o.Retry == nil {
		return c.client.Do(req)
	}
	return c.doWithRetry(req)
}

// Get 以 ctx 发起 GET 请求，响应体由调用方负责读取与关闭。
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post 以 ctx 发起 POST 请求，body 为请求体。body 为 *bytes.Buffer、
// *bytes.Reader 或 *strings.Reader 时自动可重放（可安全重试）。
// 响应体由调用方负责读取与关闭。
func (c *Client) Post(ctx context.Context, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// propagateAttrs 把 req.Context() 的日志属性写入请求头：request_id →
// X-Request-Id、user_id → X-User-Id。已存在的同名请求头不覆盖
// （显式设置的头部优先）。
func propagateAttrs(req *http.Request) {
	for _, a := range logging.AttrsFrom(req.Context()) {
		switch a.Key {
		case logging.FieldRequestID:
			if req.Header.Get(RequestIDHeader) == "" {
				req.Header.Set(RequestIDHeader, a.Value.String())
			}
		case logging.FieldUserID:
			if req.Header.Get(UserIDHeader) == "" {
				req.Header.Set(UserIDHeader, a.Value.String())
			}
		}
	}
}

// doWithRetry 按重试策略循环发送：指数退避等待，429/503 的
// Retry-After 大于退避时以 Retry-After 为准。
func (c *Client) doWithRetry(req *http.Request) (*http.Response, error) {
	ro := c.o.Retry
	exp := backoff.NewExponentialBackOff()
	exp.InitialInterval = ro.InitialInterval
	exp.MaxInterval = ro.MaxInterval

	var lastResp *http.Response
	var lastErr error
	for attempt := 1; attempt <= ro.MaxAttempts; attempt++ {
		lastResp, lastErr = c.client.Do(req)
		if lastErr != nil {
			// ctx 取消/超时无重试意义，直接返回。
			if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
				return nil, lastErr
			}
		}
		if !retriable(lastResp, lastErr) || attempt == ro.MaxAttempts {
			return lastResp, lastErr
		}
		// 带 body 且不可重放：只发送一次，不重试。
		if req.Body != nil && req.GetBody == nil {
			c.o.Logger.DebugContext(req.Context(),
				"http client: not retrying request with non-replayable body",
				"attempt", attempt, "max_attempts", ro.MaxAttempts)
			return lastResp, lastErr
		}
		// 重试间关闭上一次响应体，避免连接与资源泄漏。
		if lastResp != nil {
			lastResp.Body.Close()
		}
		// 等待退避；429/503 携带 Retry-After 时取其较大值。
		delay := exp.NextBackOff()
		if ra := retryAfter(lastResp); ra > delay {
			delay = ra
		}
		if err := wait(req.Context(), delay); err != nil {
			return nil, err
		}
		// 可重放：经 GetBody 重建请求体。
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
	}
	return lastResp, lastErr
}

// retriable 判定该尝试是否可重试：传输层错误或状态码
// 429/502/503/504。
func retriable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfter 解析 Retry-After 头（秒数或 HTTP-date）；仅 429/503
// 响应读取该头，解析失败返回 0。
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	if resp.StatusCode != http.StatusTooManyRequests &&
		resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// wait 等待 d 时长；ctx 提前结束时返回 ctx.Err()。
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
