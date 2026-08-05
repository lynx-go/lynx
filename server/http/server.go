// Package http 提供基于 gocloud.dev 的 HTTP 服务器组件，
// 内置健康检查端点、请求日志、中间件与 OpenTelemetry 插装。
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"gocloud.dev/server"
	"gocloud.dev/server/health"
)

// Default values for HTTP server configuration.
const (
	DefaultHTTPAddr        = ":8080"
	DefaultTimeout         = 60 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
)

// NewRouter 创建新的 HTTP 路由复用器。
func NewRouter() *http.ServeMux {
	return http.NewServeMux()
}

// Options 是 HTTP 服务组件的配置项。
type Options struct {
	Addr            string
	Timeout         time.Duration
	ShutdownTimeout time.Duration
	HealthCheck     lynx.HealthCheckFunc
	Logger          *slog.Logger
	RequestLog      bool
	TracerProvider  trace.TracerProvider
	MeterProvider   metric.MeterProvider
	Propagator      propagation.TextMapPropagator
	Middlewares     []Middleware
}

// Option 用于配置 HTTP 服务 Options 的选项函数。
type Option func(*Options)

// WithAddr 设置 HTTP 服务监听地址。
func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

// WithTimeout 设置 HTTP 服务的读写超时时间。
func WithTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.Timeout = timeout
	}
}

// WithShutdownTimeout 设置 HTTP 服务优雅关停的超时时间，超过后强制关闭活动连接。
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ShutdownTimeout = timeout
	}
}

// WithHealthCheck 设置 HTTP 服务的健康检查函数。
func WithHealthCheck(hc lynx.HealthCheckFunc) Option {
	return func(o *Options) {
		o.HealthCheck = hc
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

// WithTracerProvider sets the OpenTelemetry TracerProvider used by the
// server's instrumentation. When nil, the global (noop by default) provider
// is used. The provider's lifecycle (init, shutdown) is the caller's
// responsibility.
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
	*server.Server
	// mu guards the embedded *server.Server and httpServer, which are assigned
	// in Start and read in Stop; the two may run on different goroutines during
	// shutdown.
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
func (s *Server) Init(app lynx.App) error {
	return nil
}

// Start 启动 HTTP 服务并开始监听，阻塞至服务退出。
func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting HTTP server, listening on "+s.o.Addr)
	var healthChecks []health.Checker
	if s.o.HealthCheck != nil {
		healthChecks = s.o.HealthCheck()
	}
	driver := server.NewDefaultDriver()
	if s.o.Timeout > 0 {
		// Apply the configured timeout to the underlying http.Server read/write timeouts.
		driver.Server.ReadHeaderTimeout = s.o.Timeout
		driver.Server.ReadTimeout = s.o.Timeout
		driver.Server.WriteTimeout = s.o.Timeout
	}
	s.mu.Lock()
	s.httpServer = &driver.Server
	s.mu.Unlock()
	opts := &server.Options{
		HealthChecks:           healthChecks,
		TraceProvider:          s.o.TracerProvider,
		MetricsProvider:        s.o.MeterProvider,
		TraceTextMapPropagator: s.o.Propagator,
		Driver:                 driver,
	}
	if s.o.RequestLog {
		opts.RequestLogger = NewRequestLogger(s.logger, func(err error) {
			log.ErrorContext(ctx, "failed to log HTTP request", err)
		})
	}

	hs := server.New(chain(s.handler, s.o.Middlewares), opts)
	s.mu.Lock()
	s.Server = hs
	s.mu.Unlock()
	return s.ListenAndServe(s.o.Addr)
}

// Stop 优雅关停 HTTP 服务；服务尚未启动时直接返回。
// 为保证不无限挂起：调用方 context 无 deadline 时使用配置的 ShutdownTimeout，
// 超时后强制关闭活动连接（长轮询/流式 handler）。
func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping HTTP server")
	s.mu.RLock()
	srv := s.Server
	hs := s.httpServer
	s.mu.RUnlock()
	if srv == nil || hs == nil {
		return
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
			log.ErrorContext(ctx, "failed to shutdown http server", shutdownErr)
		}
	case <-ctx.Done():
		log.ErrorContext(ctx, "graceful HTTP shutdown timed out, forcing close", context.DeadlineExceeded)
		_ = hs.Close()
		<-done
	}
}

var _ lynx.Component = (*Server)(nil)
