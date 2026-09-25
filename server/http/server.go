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
	"github.com/lynx-go/lynx/internal/serverkit"
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
	// 否则 readiness 探测请求被挂起、LB 反复超时。共享规则见
	// internal/serverkit（gRPC 侧同值）。
	DefaultHealthCheckTimeout = serverkit.DefaultHealthCheckTimeout
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
	// Listener 非 nil 时 Start 直接在其上提供服务（WithListener 注入），
	// 跳过 net.Listen；Addr()/Ready() 反映该监听器。用于测试注入
	// bufconn 等非 TCP 监听器或宿主接管监听的场景。
	Listener    net.Listener
	Timeout     time.Duration
	IdleTimeout time.Duration
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
	// DisableRequestID 为 true 时不安装内置 request_id/user_id 传播中间件
	//（WithDisableRequestID）：默认安装，收到合法的 x-request-id/x-user-id
	// 即还原进请求 ctx 并回写响应头。
	DisableRequestID bool
	HealthCheckers   lynx.HealthCheckersFunc
	Logger           *slog.Logger
	RequestLog       bool
	// ErrorHandler 是服务器级默认错误处理器（WithErrorHandler 设置）：
	// 经 Server.NewErrorHandler(h, fn) 使用，h 传 nil 时的兜底改取它
	//（未配置时回退包级 DefaultErrorHandler）——包级 NewErrorHandler
	// 无法访问服务器配置，服务器级注入走该方法。
	ErrorHandler   ErrorHandler
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
	Middlewares    []Middleware
	// TLSConfig 非 nil 时以 TLS 提供服务（需包含 Certificates 或由
	// ServerOptions 填充）。
	TLSConfig *tls.Config
	// ServerOptions 透传配置底层 *http.Server（如 MaxHeaderBytes、
	// BaseContext），在内部超时配置之后应用。
	ServerOptions func(*http.Server)
	// Endpoints 是运维端点（WithEndpoint）：挂载在业务 handler 之外，不经过
	// 业务中间件 / request log / otel instrumentation。
	Endpoints []Endpoint
}

// Endpoint 是一个运维端点：路径 + 处理器（如 /metrics → promhttp、
// /debug/pprof/... → pprof.Handler）。
type Endpoint struct {
	Path    string
	Handler http.Handler
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

// WithListener 注入现成监听器：Start 跳过 net.Listen 直接在其上提供服务，
// Addr() 返回该监听器的实际地址（":0" 语义由注入监听器决定）。用于测试
// 注入 bufconn（免 TCP 端口、可并行）与宿主接管监听的场景。注意：Stop
// 会经由 Shutdown 关闭该监听器，注入方不应复用已停止的实例。
func WithListener(ln net.Listener) Option {
	return func(o *Options) {
		if ln != nil {
			o.Listener = ln
		}
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

// WithEndpoint 挂载一个运维端点（如 WithEndpoint("/metrics", telemetry.PrometheusHandler())）：
// 处理器直接挂在该路径上，不经过业务中间件、request log 与 otel
// instrumentation——与内置健康端点同一取舍：指标抓取/探针流量不产生
// 自引用指标与日志噪声。路径须以 "/" 开头且不为 "/"；与业务 handler、
// 健康端点或彼此冲突（重复/模式重叠）时 Start 返回错误，不 panic。
func WithEndpoint(path string, handler http.Handler) Option {
	return func(o *Options) {
		o.Endpoints = append(o.Endpoints, Endpoint{Path: path, Handler: handler})
	}
}

// WithDisableRequestID 关闭内置 request_id/user_id 传播中间件（默认安装）：
// 需要自行接管传播或完全不要该行为的场景使用。
func WithDisableRequestID() Option {
	return func(o *Options) {
		o.DisableRequestID = true
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

// WithErrorHandler 设置服务器级默认错误处理器：Server.NewErrorHandler
// 的 h 传 nil 时的兜底改取它（未设置时保持包级 DefaultErrorHandler）。
// 典型用法是接入服务日志实例：
//
//	WithErrorHandler(DefaultErrorHandlerWithLogger(srvLogger))
//
// 仅影响 Server.NewErrorHandler 包装的 handler；包级 NewErrorHandler
// 与显式传入非 nil h 的调用不受影响。
func WithErrorHandler(h ErrorHandler) Option {
	return func(o *Options) {
		o.ErrorHandler = h
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
	started atomic.Bool
	// stopRequested 标记 Stop 已请求（SC-02 的 HTTP 侧对称面）：Stop 在
	// 读取 httpServer 之前置位；Start 在进入 Serve 之前检查——已中断后
	// 不再 Serve，避免"Stop 见 httpServer 为 nil 先返回、Start 随后
	// Listen 并永久 Serve"的启动期交错（与 gRPC 侧同名标志语义一致）。
	stopRequested atomic.Bool
	logger        *slog.Logger
	o             Options
	handler       http.Handler
	bus           eventbus.Bus
	ready         chan struct{}
	readyOnce     sync.Once
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

	handler, err := s.buildHandler(ctx)
	if err != nil {
		// 端点配置非法（nil handler / 路径非法 / 模式冲突）：不算已启动，
		// 允许修正后重试（与 Listen 失败同一处理）。
		s.started.Store(false)
		return err
	}
	srv := &http.Server{
		Addr:              s.o.Addr,
		Handler:           handler,
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

	ln := s.o.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.o.Addr)
		if err != nil {
			// Listen 失败不算已启动：复位守卫，允许换地址重试。
			s.started.Store(false)
			return err
		}
	}
	// 监听就绪后才打印 listening 日志（SC-16）：提前打印会在 Listen 失败
	// （如端口占用）时留下误导性的"正在监听"记录，且与 listening 事件
	// 的语义对齐。
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	// Stop 已先行（启动期中断交错）：不进入 Serve。Stop 若在 httpServer
	// 存储之后执行，其 Shutdown 会经 srv.inShutdown 让 Serve 立即返回
	// ErrServerClosed（下方归一化兜底）；此处覆盖 Stop 见 httpServer 为
	// nil 先返回的交错——关闭监听器（含 WithListener 注入的实例）后返回。
	if s.stopRequested.Load() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.WarnContext(ctx, "error closing listener after interrupted start", "error", err)
		}
		return nil
	}
	s.logger.InfoContext(ctx, "starting HTTP server, listening on "+ln.Addr().String())
	serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerListening, "http", ln.Addr().String(), s.o.AdvertiseAddr)
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

// buildHandler 组装完整请求处理链：健康端点与运维端点（WithEndpoint）独立
// 挂载（不经过 otel/requestlog/业务中间件），业务 handler 按 request log →
// 中间件 → otel 顺序包装。端点配置非法（nil handler / 路径非法 / 模式冲突）
// 返回错误，由 Start 上抛。
func (s *Server) buildHandler(ctx context.Context) (http.Handler, error) {
	mux := http.NewServeMux()
	if !s.o.DisableHealthCheck {
		s.mountHealthEndpoints(mux)
	}
	for _, ep := range s.o.Endpoints {
		if ep.Handler == nil {
			return nil, fmt.Errorf("http server: endpoint %q has nil handler", ep.Path)
		}
		if !strings.HasPrefix(ep.Path, "/") || ep.Path == "/" {
			return nil, fmt.Errorf("http server: endpoint path %q must start with \"/\" and must not be \"/\"", ep.Path)
		}
		if err := mountEndpoint(mux, ep); err != nil {
			return nil, err
		}
	}

	user := chain(s.handler, s.o.Middlewares)
	// 默认安装 request_id/user_id 传播（WithDisableRequestID 关闭）：
	// 客户端已在发 x-request-id/x-user-id，服务端默认还原进 ctx，闭环
	// 不依赖用户记得加中间件。
	if !s.o.DisableRequestID {
		user = WithRequestID()(user)
	}
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
	return mux, nil
}

// mountEndpoint 注册运维端点，把 ServeMux 的模式冲突 panic 翻译为错误
// （重复路径、与健康端点或彼此模式重叠等）。
func mountEndpoint(mux *http.ServeMux, ep Endpoint) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("http server: mount endpoint %q: %v", ep.Path, r)
		}
	}()
	mux.Handle(ep.Path, ep.Handler)
	return nil
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
		if err := serverkit.RunHealthChecks(checkers, timeout); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// Stop 优雅关停 HTTP 服务；服务尚未启动时直接返回 nil。
// 有界规则（SC-05，与 gRPC/debug 共用 internal/serverkit）：调用方 ctx
// deadline 与 ShutdownTimeout 并存时取较小者；ShutdownTimeout=0 表示无
// 配置上界、仅以调用方 ctx 为准；超时后强制关闭活动连接（长轮询/流式
// handler），并以错误返回。
func (s *Server) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping HTTP server")
	serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerStopping, "http", s.Addr(), s.o.AdvertiseAddr)
	// 读取 httpServer 之前置位（SC-02 的 HTTP 侧对称面）：Start 侧据此在
	// 进入 Serve 前中止，避免 Stop 见 httpServer 为 nil 先返回、Start
	// 随后 Listen 并永久 Serve 的启动期交错。
	s.stopRequested.Store(true)
	s.mu.RLock()
	hs := s.httpServer
	s.mu.RUnlock()
	if hs == nil {
		serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerStopped, "http", "", "")
		return nil
	}
	defer serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerStopped, "http", "", "")
	graceful := func(ctx context.Context) error {
		err := hs.Shutdown(ctx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	return serverkit.Shutdown(ctx, s.o.ShutdownTimeout, s.logger, "http server", graceful, hs.Close)
}

var _ lynx.Service = (*Server)(nil)

var _ lynx.Ready = (*Server)(nil)

var _ lynx.Server = (*Server)(nil)
