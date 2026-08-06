// Package grpc 提供 gRPC 服务器组件，内置日志/恢复拦截器、
// 健康检查与反射服务，支持 OpenTelemetry 插装。
package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/grpc/interceptor"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// Default values for gRPC server configuration.
const (
	DefaultGRPCAddr          = ":9090"
	DefaultTimeout           = 60 * time.Second
	DefaultHealthCheckPeriod = 10 * time.Second
)

// Options 是 gRPC 服务组件的配置项。
type Options struct {
	Addr                string
	Timeout             time.Duration
	Logger              *slog.Logger
	Interceptors        []grpc.UnaryServerInterceptor
	StreamInterceptors  []grpc.StreamServerInterceptor
	ServerOptions       []grpc.ServerOption
	TracerProvider      trace.TracerProvider
	MeterProvider       metric.MeterProvider
	// HealthCheck 提供 app 级健康检查器；非 nil 时按 HealthCheckPeriod
	// 轮询并同步到 grpc health 服务（依赖组件不健康时探测返回 NOT_SERVING）。
	HealthCheck         lynx.HealthCheckersFunc
	HealthCheckPeriod   time.Duration
}

// Option 用于配置 gRPC 服务 Options 的选项函数。
type Option func(*Options)

// WithAddr 设置 gRPC 服务监听地址。
func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

// WithTimeout 设置 gRPC 服务优雅关停的超时时间。
func WithTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.Timeout = timeout
	}
}

// WithLogger 设置 gRPC 服务的日志实例。
// 注意：请求拦截器路径的日志走本 logger；Start/Stop 路径的日志经
// s.logger 输出，与 WithLogger 的实例一致。
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithInterceptors 追加一元 RPC 服务端拦截器，在内置恢复与日志拦截器之后执行。
func WithInterceptors(interceptors ...grpc.UnaryServerInterceptor) Option {
	return func(o *Options) {
		o.Interceptors = append(o.Interceptors, interceptors...)
	}
}

// WithStreamInterceptors 追加流式 RPC 服务端拦截器，在内置恢复与日志拦截器之后执行。
func WithStreamInterceptors(interceptors ...grpc.StreamServerInterceptor) Option {
	return func(o *Options) {
		o.StreamInterceptors = append(o.StreamInterceptors, interceptors...)
	}
}

// WithHealthCheck 设置 app 级健康检查函数，轮询结果同步到 grpc health 服务。
func WithHealthCheck(hc lynx.HealthCheckersFunc) Option {
	return func(o *Options) {
		o.HealthCheck = hc
	}
}

// WithHealthCheckPeriod 设置 app 级健康检查的轮询间隔。
func WithHealthCheckPeriod(period time.Duration) Option {
	return func(o *Options) {
		o.HealthCheckPeriod = period
	}
}

// WithServerOptions 透传额外的 grpc.ServerOption（如 TLS 凭据、消息大小限制、
// keepalive、最大并发流等），在内部选项之后应用到 grpc.NewServer。
func WithServerOptions(options ...grpc.ServerOption) Option {
	return func(o *Options) {
		o.ServerOptions = append(o.ServerOptions, options...)
	}
}

// WithTracerProvider sets the OpenTelemetry TracerProvider used by the
// server's stats handler. When nil, the global (noop by default) provider is
// used. The provider's lifecycle is the caller's responsibility.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

// WithMeterProvider sets the OpenTelemetry MeterProvider used by the server's
// stats handler. When nil, the global (noop by default) provider is used.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *Options) {
		o.MeterProvider = mp
	}
}

// NewServer 创建 gRPC 服务组件，内置日志、恢复拦截器与 OpenTelemetry stats handler，
// 并注册 gRPC 健康检查服务。
func NewServer(opts ...Option) *Server {
	options := Options{
		Addr:    DefaultGRPCAddr,
		Timeout: DefaultTimeout,
		Logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(&options)
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	// 零值/负值 Period 回退缺省值：time.NewTicker(0) 会 panic，
	// 轮询 goroutine 不得使用未经装配的零值。
	if options.HealthCheckPeriod <= 0 {
		options.HealthCheckPeriod = DefaultHealthCheckPeriod
	}

	s := &Server{
		logger: options.Logger,
		o:      options,
	}
	// Recovery 在最外层：链内任意一环（含用户拦截器）panic 都能被恢复。
	unaryInterceptors := []grpc.UnaryServerInterceptor{
		interceptor.Recovery(),
		interceptor.Logging(s.logger),
	}
	unaryInterceptors = append(unaryInterceptors, options.Interceptors...)
	// 流式 RPC 同样需要日志与 panic 恢复：gRPC 对流式 handler 的 panic
	// 没有内置保护，不加拦截器会直接崩溃整个进程。
	streamInterceptors := []grpc.StreamServerInterceptor{
		interceptor.RecoveryStream(),
		interceptor.LoggingStream(s.logger),
	}
	streamInterceptors = append(streamInterceptors, options.StreamInterceptors...)
	statsOpts := []otelgrpc.Option{}
	if options.TracerProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithTracerProvider(options.TracerProvider))
	}
	if options.MeterProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithMeterProvider(options.MeterProvider))
	}
	grpcOpts := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(statsOpts...)),
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(streamInterceptors...),
	}
	grpcOpts = append(grpcOpts, options.ServerOptions...)

	s.server = grpc.NewServer(grpcOpts...)

	// Register health check service
	s.health = health.NewServer()
	grpc_health_v1.RegisterHealthServer(s.server, s.health)
	return s
}

// Server 是 gRPC 服务组件，实现 lynx.Service 接口。
type Server struct {
	// mu guards listener, which is written by Start and read by Stop on a
	// different goroutine during shutdown.
	mu           sync.Mutex
	server       *grpc.Server
	listener     net.Listener
	logger       *slog.Logger
	o            Options
	health       *health.Server
	healthCancel context.CancelFunc
	// stopped 标记 Stop 已被调用（mu 保护）：Stop 早于 Start 执行到
	// startHealthPoller 时，poller 启动即在同锁段内发现并取消自身，
	// 不会泄漏无人取消的轮询 goroutine。
	stopped bool
	running atomic.Bool
}

// CheckHealth 实现健康检查，服务未处于运行状态时返回错误。
func (s *Server) CheckHealth() error {
	if !s.running.Load() {
		return grpc.ErrServerStopped
	}
	// Check if the server is still serving
	return nil
}

// Name 返回组件名称 "grpc"。
func (s *Server) Name() string {
	return "grpc"
}

// Init 初始化组件，gRPC 服务无需在初始化阶段做额外工作。
func (s *Server) Init(ctx lynx.AppContext) error {
	return nil
}

// Start 启动 gRPC 服务并开始监听，阻塞至服务退出。
func (s *Server) Start(ctx context.Context) error {
	s.logger.InfoContext(ctx, "starting gRPC server, listening on "+s.o.Addr)

	lis, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener = lis
	s.mu.Unlock()

	// Set the server to healthy, for both the named and the standard empty
	// service name used by most gRPC health probes.
	s.health.SetServingStatus("grpc", grpc_health_v1.HealthCheckResponse_SERVING)
	s.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register reflection service
	reflection.Register(s.server)

	s.running.Store(true)
	// Serve 返回（监听失败/优雅停止）后复位运行标志，CheckHealth 不再报健康。
	defer s.running.Store(false)

	if s.o.HealthCheck != nil {
		s.startHealthPoller()
	}
	return s.server.Serve(lis)
}

// startHealthPoller 按 HealthCheckPeriod 轮询 app 级健康检查器并同步到
// grpc health 服务：任一依赖组件不健康时探测返回 NOT_SERVING。
// Stop 若已先于 Start 执行（healthCancel 为 nil 未被取走），此处在同一
// 持锁段内发现 stopped 即取消并返回，不启动轮询 goroutine（无泄漏）。
func (s *Server) startHealthPoller() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		cancel()
		return
	}
	s.healthCancel = cancel
	s.mu.Unlock()
	go func() {
		s.updateHealthStatus()
		t := time.NewTicker(s.o.HealthCheckPeriod)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.updateHealthStatus()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Server) updateHealthStatus() {
	status := grpc_health_v1.HealthCheckResponse_SERVING
	for _, c := range s.o.HealthCheck() {
		if err := c.CheckHealth(); err != nil {
			status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
			break
		}
	}
	s.health.SetServingStatus("", status)
	s.health.SetServingStatus("grpc", status)
}

// Stop 优雅关停 gRPC 服务：先关闭监听器，再等待在途请求完成，超时后强制停止。
// 返回错误（如强制停止）使调用方感知关停失败。
func (s *Server) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping gRPC server")
	if s.health != nil {
		s.health.SetServingStatus("grpc", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		s.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	}
	s.running.Store(false)
	s.mu.Lock()
	s.stopped = true
	if s.healthCancel != nil {
		s.healthCancel()
		s.healthCancel = nil
	}
	s.mu.Unlock()

	// Close the listener first to stop accepting new connections
	s.mu.Lock()
	lis := s.listener
	s.listener = nil
	s.mu.Unlock()
	if lis != nil {
		_ = lis.Close()
	}

	if s.server == nil {
		return nil
	}

	// The configured Timeout is an upper bound on graceful stop: use it even
	// when the caller's context already has a deadline, taking the smaller of
	// the two.
	if s.o.Timeout > 0 {
		var cancel context.CancelFunc
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining < s.o.Timeout {
				ctx, cancel = context.WithTimeout(ctx, remaining)
			} else {
				ctx, cancel = context.WithTimeout(ctx, s.o.Timeout)
			}
		} else {
			ctx, cancel = context.WithTimeout(ctx, s.o.Timeout)
		}
		defer cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.server.GracefulStop()
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "gRPC server stopped gracefully")
		return nil
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "graceful stop timeout, forcing stop")
		s.server.Stop()
		<-done
		return fmt.Errorf("gRPC server graceful stop timed out")
	}
}

// GetServer 返回底层 *grpc.Server 实例，用于注册业务服务实现。
func (s *Server) GetServer() *grpc.Server {
	return s.server
}

var _ lynx.Service = (*Server)(nil)
