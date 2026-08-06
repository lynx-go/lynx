// Package http 提供 HTTP 服务器组件，内置健康检查端点、请求日志、
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
	"sync"
	"time"

	"github.com/lynx-go/lynx"
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
)

// Options 是 HTTP 服务组件的配置项。
type Options struct {
	Addr            string
	Timeout         time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	HealthCheckers  lynx.HealthCheckersFunc
	Logger          *slog.Logger
	RequestLog      bool
	TracerProvider  trace.TracerProvider
	MeterProvider   metric.MeterProvider
	Propagator      propagation.TextMapPropagator
	Middlewares     []Middleware
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

// WithShutdownTimeout 设置 HTTP 服务优雅关停的超时时间，超过后强制关闭活动连接。
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ShutdownTimeout = timeout
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

// NewServer 创建 HTTP 服务组件，使用给定的 handler 与配置项。
func NewServer(handler http.Handler, opts ...Option) *Server {
	options := Options{
		Addr:            DefaultHTTPAddr,
		Timeout:         DefaultTimeout,
		ShutdownTimeout: DefaultShutdownTimeout,
		Logger:          slog.Default(),
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
	}
}

// Server 是 HTTP 服务组件，实现 lynx.Component 接口。
type Server struct {
	// mu guards httpServer, which is assigned in Start and read in Stop;
	// the two may run on different goroutines during shutdown.
	mu         sync.RWMutex
	httpServer *http.Server
	logger     *slog.Logger
	o          Options
	handler    http.Handler
}

// Name 返回组件名称 "http"。
func (s *Server) Name() string {
	return "http"
}

// Init 初始化组件，HTTP 服务无需在初始化阶段做额外工作。
func (s *Server) Init(env lynx.Env) error {
	return nil
}

// Start 启动 HTTP 服务并开始监听，阻塞至服务退出。
// 显式注入 otel provider（WithPublicEndpoint 保持与旧实现一致的
// traceparent-as-link 语义），不修改进程全局 otel provider。
func (s *Server) Start(ctx context.Context) error {
	s.logger.InfoContext(ctx, "starting HTTP server, listening on "+s.o.Addr)

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
		return err
	}
	if s.o.TLSConfig != nil {
		srv.TLSConfig = s.o.TLSConfig
		return srv.ServeTLS(ln, "", "")
	}
	return srv.Serve(ln)
}

// buildHandler 组装完整请求处理链：健康端点独立挂载（不经过
// otel/requestlog），业务 handler 按 request log → 中间件 → otel 顺序包装。
func (s *Server) buildHandler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz/liveness", handleLiveness)
	mux.Handle("/healthz/readiness", handleReadiness(s.o.HealthCheckers))

	user := chain(s.handler, s.o.Middlewares)
	if s.o.RequestLog {
		user = NewHandler(NewRequestLogger(s.logger, func(err error) {
			s.logger.ErrorContext(ctx, "failed to log HTTP request", "error", err)
		}), user)
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

// handleLiveness 存活检查：进程存活即返回 200。
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleReadiness 就绪检查：依次调用全部健康检查器，任一失败返回
// 503 + 错误正文；未配置检查器时恒 200。
func handleReadiness(checkers lynx.HealthCheckersFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkers == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		for _, c := range checkers() {
			if err := c.CheckHealth(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

// Stop 优雅关停 HTTP 服务；服务尚未启动时直接返回 nil。
// 为保证不无限挂起：调用方 context 无 deadline 时使用配置的 ShutdownTimeout，
// 超时后强制关闭活动连接（长轮询/流式 handler），并以错误返回。
func (s *Server) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping HTTP server")
	s.mu.RLock()
	hs := s.httpServer
	s.mu.RUnlock()
	if hs == nil {
		return nil
	}
	if _, ok := ctx.Deadline(); !ok && s.o.ShutdownTimeout > 0 {
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
		if shutdownErr != nil &&
			!errors.Is(shutdownErr, http.ErrServerClosed) &&
			!errors.Is(shutdownErr, context.Canceled) &&
			!errors.Is(shutdownErr, context.DeadlineExceeded) {
			s.logger.ErrorContext(ctx, "failed to shutdown http server", "error", shutdownErr)
			return shutdownErr
		}
		return nil
	case <-ctx.Done():
		s.logger.ErrorContext(ctx, "graceful HTTP shutdown timed out, forcing close", "error", context.DeadlineExceeded)
		_ = hs.Close()
		<-done
		return fmt.Errorf("http server graceful shutdown timed out")
	}
}

var _ lynx.Component = (*Server)(nil)
