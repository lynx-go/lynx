// Package http 提供 HTTP 服务器服务，内置健康检查端点、请求日志、
// 中间件与 OpenTelemetry 插装（显式注入 provider，无进程全局副作用）。
package http

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Default values for HTTP server configuration.
const (
	DefaultHTTPAddr        = ":8080"
	DefaultTimeout         = 60 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
	// DefaultHealthCheckTimeout 是单个健康检查器的执行上限（SC-03）：
	// CheckHealth 接口无 ctx 参数，挂死的 checker 必须被限时打断，
	// 否则 readiness 探测请求被挂起、LB 反复超时。
	DefaultHealthCheckTimeout = 3 * time.Second
	// DefaultHealthCheckPrefix 是内置健康检查端点的路径前缀（SC-08）：
	// 端点为 <prefix>/liveness 与 <prefix>/readiness。
	DefaultHealthCheckPrefix = "/healthz"
)

// Options 是 HTTP 服务服务的配置项。
type Options struct {
	Addr string
	// AdvertiseAddr 是服务对外宣告的地址（host:port），由
	// WithAdvertiseAddr 设置，仅原样保存该字符串；为空表示未显式指定。
	AdvertiseAddr string
	Timeout       time.Duration
	IdleTimeout   time.Duration
	// ShutdownTimeout 是优雅关停的上限；0 表示无上界（仅受调用方 Stop
	// ctx 约束，SC-17），与调用方 deadline 并存时取较小者（SC-05）。
	ShutdownTimeout time.Duration
	// HealthCheckTimeout 是 readiness 检查中单个 checker 的执行上限，
	// 超时视为不健康（SC-03）；0 表示不限时（慎用，挂死 checker 会挂起
	// 探测请求）。
	HealthCheckTimeout time.Duration
	// HealthCheckPrefix 是健康检查端点前缀（缺省 /healthz，SC-08）。
	HealthCheckPrefix string
	// DisableHealthCheck 为 true 时不挂载内置健康检查端点（SC-08）。
	DisableHealthCheck bool
	HealthCheckers     lynx.HealthCheckersFunc
	Logger             *slog.Logger
	RequestLog         bool
	TracerProvider     trace.TracerProvider
	MeterProvider      metric.MeterProvider
	Propagator         propagation.TextMapPropagator
	Middlewares        []Middleware
	// TLSConfig 非 nil 时以 TLS 提供服务（需包含 Certificates 或由
	// ServerOptions 填充）。
	TLSConfig *tls.Config
	// ServerOptions 透传配置底层 *http.Server（如 MaxHeaderBytes、
	// BaseContext），在内部超时配置之后应用。
	ServerOptions func(*http.Server)
}

// Option 用于配置 HTTP 服务 Options 的选项函数。
type Option func(*Options)

// WithAddr 设置 HTTP 服务监听地址。
func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

// WithAdvertiseAddr 设置 HTTP 服务对外宣告的地址（host:port），仅保存该
// 字符串，供注册发现等场景经 AdvertiseAddr 读取；不影响实际监听地址，
// 也不参与协议推断。
func WithAdvertiseAddr(hostPort string) Option {
	return func(o *Options) {
		o.AdvertiseAddr = hostPort
	}
}

// WithTimeout 设置 HTTP 服务的读写超时时间（同时作用于
// ReadHeaderTimeout/ReadTimeout/WriteTimeout）。
func WithTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.Timeout = timeout
	}
}

// WithIdleTimeout 设置 HTTP 连接的空闲超时时间。
func WithIdleTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.IdleTimeout = timeout
	}
}

// WithShutdownTimeout 设置 HTTP 服务优雅关停的超时时间，超过后强制关闭
// 活动连接。注意（SC-17）：传 0 表示显式禁用上界——Stop 仅受调用方 ctx
// 约束，活动连接不返回时 Stop 可无限等待；生产环境建议保留上界。
// 与调用方 ctx deadline 并存时取较小者（SC-05，与 gRPC 侧 Stop 一致）。
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ShutdownTimeout = timeout
	}
}

// WithHealthCheckTimeout 设置 readiness 检查中单个健康检查器的执行上限
// （缺省 3s）：CheckHealth 接口无 ctx/超时（API 冻结），挂死的 checker
// 在超时后被视为不健康，而不是挂起探测请求（SC-03）。传 0 表示不限时。
func WithHealthCheckTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.HealthCheckTimeout = timeout
	}
}

// WithHealthCheckPrefix 设置内置健康检查端点的路径前缀（缺省 /healthz，
// 端点为 <prefix>/liveness 与 <prefix>/readiness，SC-08）：用于网关路径
// 规划或与其他框架的端点对齐。
func WithHealthCheckPrefix(prefix string) Option {
	return func(o *Options) {
		o.HealthCheckPrefix = prefix
	}
}

// WithDisableHealthCheck 完全关闭内置健康检查端点（SC-08）：健康探测
// 交给外部网关/独立探针进程时使用。
func WithDisableHealthCheck() Option {
	return func(o *Options) {
		o.DisableHealthCheck = true
	}
}

// WithHealthCheckers 设置 HTTP 服务的健康检查器取值函数。
// 通常传方法值 app.HealthCheckers。
func WithHealthCheckers(hc lynx.HealthCheckersFunc) Option {
	return func(o *Options) {
		o.HealthCheckers = hc
	}
}

// WithLogger 设置 HTTP 服务的日志实例。
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithRequestLog 设置是否记录 HTTP 请求日志。
func WithRequestLog(requestLog bool) Option {
	return func(o *Options) {
		o.RequestLog = requestLog
	}
}

// WithTLSConfig 以 TLS 提供服务，cfg 需包含 Certificates 或由
// WithServerOptions 填充。
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *Options) {
		o.TLSConfig = cfg
	}
}

// WithServerOptions 透传配置底层 *http.Server，在内部超时之后应用。
func WithServerOptions(fn func(*http.Server)) Option {
	return func(o *Options) {
		o.ServerOptions = fn
	}
}

// WithTracerProvider sets the OpenTelemetry TracerProvider used by the
// server's instrumentation. When nil, the global (noop by default) provider
// is used. The provider's lifecycle (init, shutdown) is the caller's
// responsibility. 显式注入，不修改进程全局 provider。
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

// WithMeterProvider sets the OpenTelemetry MeterProvider used by the server's
// instrumentation. When nil, the global (noop by default) provider is used.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *Options) {
		o.MeterProvider = mp
	}
}

// WithPropagator sets the propagator used to extract trace context from
// incoming requests. When nil, the global propagator is used.
func WithPropagator(p propagation.TextMapPropagator) Option {
	return func(o *Options) {
		o.Propagator = p
	}
}

// NewServer 创建 HTTP 服务服务，使用给定的 handler 与配置项。
func NewServer(handler http.Handler, opts ...Option) *Server {
	options := Options{
		Addr:               DefaultHTTPAddr,
		Timeout:            DefaultTimeout,
		ShutdownTimeout:    DefaultShutdownTimeout,
		HealthCheckTimeout: DefaultHealthCheckTimeout,
		HealthCheckPrefix:  DefaultHealthCheckPrefix,
		Logger:             slog.Default(),
	}
	for _, opt := range opts {
		opt(&options)
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	return &Server{
		logger:  options.Logger,
		o:       options,
		handler: handler,
		ready:   make(chan struct{}),
	}
}

// Server 是 HTTP 服务，实现 lynx.Service 接口。
type Server struct {
	// mu guards httpServer and listener, which are assigned in Start and
	// read in Stop/Addr; the two may run on different goroutines during
	// shutdown.
	mu         sync.RWMutex
	httpServer *http.Server
	listener   net.Listener
	// started 守卫 Start 重入（SC-14）：二次 Start 会覆盖 httpServer/
	// listener 造成旧 listener 泄漏，必须直接报错。Init 会复位本标志
	//（SC-15，重启语义留口；当前生命周期内不支持 restart）。
	started   atomic.Bool
	logger    *slog.Logger
	o         Options
	handler   http.Handler
	bus       eventbus.Bus
	ready     chan struct{}
	readyOnce sync.Once
}

// Name 返回服务名称 "http"。
func (s *Server) Name() string {
	return "http"
}

// Init 初始化服务：接管 Bus，并复位 Start 守卫标志（SC-15：lynx 框架
// 生命周期为 Init→Start→Stop，Init 复位使"重新 Init 后可再 Start"留有
// 语义口子；当前不支持同一实例不重 Init 的 restart）。
func (s *Server) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		s.bus = ctx.Bus()
	}
	s.started.Store(false)
	return nil
}

func (s *Server) publishEvent(topic string, payload any) {
	if s.bus == nil {
		return
	}
	_ = s.bus.Publish(context.Background(), topic, payload)
}

// Addr 返回实际监听地址：Start 前（或 Listen 失败时）返回空字符串；
// 使用随机端口（如 ":0"）时返回 Listen 成功后的实际地址。
// 语义与 debug.Service.Addr 一致。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// AdvertiseAddr 返回 WithAdvertiseAddr 设置的宣告地址；未设置时返回
// 空字符串。
func (s *Server) AdvertiseAddr() string {
	return s.o.AdvertiseAddr
}

// Ready 在 Listen 成功之后、Serve 之前关闭。Listen 失败不关闭。
func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

func (s *Server) closeReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

// Start 启动 HTTP 服务并开始监听，阻塞至服务退出。
// 显式注入 otel provider（WithPublicEndpoint 保持与旧实现一致的
// traceparent-as-link 语义），不修改进程全局 otel provider。
//
// 正常关停（Stop/Shutdown）返回 nil：Serve 返回的 http.ErrServerClosed
// 归一化为 nil（SC-02），避免框架把正常关停发布为 lynx.service.failed
// 虚假事件。二次调用 Start 返回错误（SC-14）。
func (s *Server) Start(ctx context.Context) error {
	// 重入守卫：二次 Start 会覆盖 httpServer/listener 并泄漏旧 listener。
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("http server: Start called more than once")
	}

	srv := &http.Server{
		Addr:              s.o.Addr,
		Handler:           s.buildHandler(ctx),
		ReadHeaderTimeout: s.o.Timeout,
		ReadTimeout:       s.o.Timeout,
		WriteTimeout:      s.o.Timeout,
		IdleTimeout:       s.o.IdleTimeout,
	}
	if s.o.ServerOptions != nil {
		s.o.ServerOptions(srv)
	}
	s.mu.Lock()
	s.httpServer = srv
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		// Listen 失败不算已启动：复位守卫，允许换地址重试。
		s.started.Store(false)
		return err
	}
	// 监听就绪后才打印 listening 日志（SC-16）：提前打印会在 Listen 失败
	// （如端口占用）时留下误导性的"正在监听"记录，且与 listening 事件
	// 的语义对齐。
	s.logger.InfoContext(ctx, "starting HTTP server, listening on "+ln.Addr().String())
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	s.publishEvent(eventbus.TopicHTTPListening, eventbus.ServerEvent{Service: "http", Addr: ln.Addr().String(), AdvertiseAddr: s.o.AdvertiseAddr, Time: time.Now()})
	s.closeReady()
	var serveErr error
	if s.o.TLSConfig != nil {
		srv.TLSConfig = s.o.TLSConfig
		serveErr = srv.ServeTLS(ln, "", "")
	} else {
		serveErr = srv.Serve(ln)
	}
	// Shutdown/Close 后 Serve 返回 http.ErrServerClosed：这是正常关停的
	// 一部分而非失败，归一化为 nil（SC-02）。
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// buildHandler 组装完整请求处理链：健康端点独立挂载（不经过
// otel/requestlog），业务 handler 按 request log → 中间件 → otel 顺序包装。
func (s *Server) buildHandler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	if !s.o.DisableHealthCheck {
		s.mountHealthEndpoints(mux)
	}

	user := chain(s.handler, s.o.Middlewares)
	if s.bus != nil {
		user = injectBus(s.bus)(user)
	}
	if s.o.RequestLog {
		// onErr 恒不触发（slog 写入无法失败，SC-09 已移除死回调链），
		// 传 nil 保持 NewRequestLogger 既有签名兼容。
		user = NewHandler(NewRequestLogger(s.logger, nil), user)
	}
	otelOpts := []otelhttp.Option{
		// 入站 traceparent 提取为 span link 而非父 span。
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
	}
	if s.o.TracerProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithTracerProvider(s.o.TracerProvider))
	}
	if s.o.MeterProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithMeterProvider(s.o.MeterProvider))
	}
	if s.o.Propagator != nil {
		otelOpts = append(otelOpts, otelhttp.WithPropagators(s.o.Propagator))
	}
	user = otelhttp.NewHandler(user, "", otelOpts...)
	mux.Handle("/", user)
	return mux
}

// mountHealthEndpoints 挂载内置健康检查端点（<prefix>/liveness 与
// <prefix>/readiness）。端点刻意绕过 otel/requestlog/用户中间件：探测
// 流量不产生 span/访问日志（降噪），也不受业务中间件（如限流）误杀——
// 这是合理取舍；但 checker panic 仍需兜底（SC-08）：外包 Recovery，
// panic → 500 + 日志，不拖垮进程。
func (s *Server) mountHealthEndpoints(mux *http.ServeMux) {
	prefix := s.o.HealthCheckPrefix
	if prefix == "" {
		prefix = DefaultHealthCheckPrefix
	}
	// 规格化前缀：保证以 "/" 开头、不以 "/" 结尾（ServeMux 路径匹配要求）。
	prefix = strings.TrimSuffix(prefix, "/")
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	readiness := Recovery()(handleReadiness(s.o.HealthCheckers, s.o.HealthCheckTimeout))
	mux.HandleFunc(prefix+"/liveness", handleLiveness)
	mux.Handle(prefix+"/readiness", readiness)
}

// handleLiveness 存活检查：进程存活即返回 200。
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleReadiness 就绪检查：并发调用全部健康检查器（每个限时），任一
// 失败或超时返回 503 + 错误正文；未配置检查器时恒 200。
func handleReadiness(checkers lynx.HealthCheckersFunc, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkers == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := runHealthChecks(checkers, timeout); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// runHealthChecks 并发执行 checkers 并整体限时 timeout：任一失败立即
// 返回其错误；超时返回超时错误（视为不健康，SC-03）。
// lynx.Checker 接口无 ctx 参数（API 冻结），挂死的 checker 无法被打断，
// 只能被"放弃等待"——结果 channel 带缓冲，最终返回的 checker goroutine
// 写入后自行退出，不阻塞探测请求。固有边界（放弃等待模式的残余代价）：
// 永不返回的 checker（死锁类）其 goroutine 无法回收，会随每次探测累积
// 泄漏，只能修复 checker 本身或重启进程消除。
// timeout <= 0 表示不限时：退化为顺序执行（保持旧行为的逃生口）。
func runHealthChecks(checkers lynx.HealthCheckersFunc, timeout time.Duration) error {
	cs := checkers()
	if len(cs) == 0 {
		return nil
	}
	if timeout <= 0 {
		for _, c := range cs {
			if err := checkOne(c); err != nil {
				return err
			}
		}
		return nil
	}
	results := make(chan error, len(cs))
	for _, c := range cs {
		go func(c lynx.Checker) {
			results <- checkOne(c)
		}(c)
	}
	// checker 并发起步，共享一个计时窗口即等效"每个 checker 限时"。
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for i := 0; i < len(cs); i++ {
		select {
		case err := <-results:
			if err != nil {
				return err
			}
		case <-timer.C:
			return fmt.Errorf("health check timed out after %s", timeout)
		}
	}
	return nil
}

// checkOne 执行单个 checker 并兜底其 panic：并发路径下 checker 运行在
// 独立 goroutine，panic 无外层中间件可恢复（SC-08 的 Recovery 只覆盖
// handler goroutine），必须就地 recover 并按不健康处理，避免拖垮进程。
func checkOne(c lynx.Checker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("health check panicked: %v", r)
		}
	}()
	return c.CheckHealth()
}

// Stop 优雅关停 HTTP 服务；服务尚未启动时直接返回 nil。
// 为保证不无限挂起：与 gRPC 侧 Stop 一致地取 min 语义（SC-05）——
// 调用方 ctx deadline 与 ShutdownTimeout 并存时取较小者（context
// 会自动取父 ctx 的较早 deadline），ShutdownTimeout=0 表示无上界、仅以
// 调用方 ctx 为准；超时后强制关闭活动连接（长轮询/流式 handler），
// 并以错误返回。
func (s *Server) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping HTTP server")
	s.publishEvent(eventbus.TopicHTTPStopping, eventbus.ServerEvent{Service: "http", Addr: s.Addr(), AdvertiseAddr: s.o.AdvertiseAddr, Time: time.Now()})
	s.mu.RLock()
	hs := s.httpServer
	s.mu.RUnlock()
	if hs == nil {
		s.publishEvent(eventbus.TopicHTTPStopped, eventbus.ServerEvent{Service: "http", Time: time.Now()})
		return nil
	}
	defer s.publishEvent(eventbus.TopicHTTPStopped, eventbus.ServerEvent{Service: "http", Time: time.Now()})
	if s.o.ShutdownTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.o.ShutdownTimeout)
		defer cancel()
	}
	done := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(done)
		shutdownErr = hs.Shutdown(ctx)
	}()
	select {
	case <-done:
		switch {
		case shutdownErr == nil, errors.Is(shutdownErr, http.ErrServerClosed):
			// 正常优雅关停。
			return nil
		case errors.Is(shutdownErr, context.Canceled), errors.Is(shutdownErr, context.DeadlineExceeded):
			// Shutdown 因超时/取消返回：与 ctx.Done() 分支同样归入超时
			// 路径，强制关闭活动连接并以错误返回——不得放行未完成的连接。
			s.logger.ErrorContext(ctx, "graceful HTTP shutdown timed out, forcing close", "error", shutdownErr)
			if err := hs.Close(); err != nil {
				s.logger.ErrorContext(ctx, "failed to force-close http server after shutdown timeout", "error", err)
			}
			return fmt.Errorf("http server graceful shutdown timed out: %w", shutdownErr)
		default:
			s.logger.ErrorContext(ctx, "failed to shutdown http server", "error", shutdownErr)
			return shutdownErr
		}
	case <-ctx.Done():
		// ctx 已结束，Shutdown 必然很快返回；先等它退出再强制关闭，
		// 避免与 Shutdown 内部的连接清理交错。
		<-done
		s.logger.ErrorContext(ctx, "graceful HTTP shutdown timed out, forcing close", "error", ctx.Err())
		if err := hs.Close(); err != nil {
			s.logger.ErrorContext(ctx, "failed to force-close http server after shutdown timeout", "error", err)
		}
		return fmt.Errorf("http server graceful shutdown timed out: %w", ctx.Err())
	}
}

var _ lynx.Service = (*Server)(nil)

var _ lynx.Ready = (*Server)(nil)
