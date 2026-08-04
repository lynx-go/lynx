package grpc

import (
	"context"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/grpc/interceptor"
	"github.com/lynx-go/x/log"
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
	DefaultGRPCAddr = ":9090"
	DefaultTimeout  = 60 * time.Second
)

// Options 是 gRPC 服务组件的配置项。
type Options struct {
	Addr           string
	Timeout        time.Duration
	Logger         *slog.Logger
	Interceptors   []grpc.UnaryServerInterceptor
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
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
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithInterceptors 追加一元 RPC 服务端拦截器，在内置日志与恢复拦截器之后执行。
func WithInterceptors(interceptors ...grpc.UnaryServerInterceptor) Option {
	return func(o *Options) {
		o.Interceptors = append(o.Interceptors, interceptors...)
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

	s := &Server{
		logger: options.Logger,
		o:      options,
	}
	interceptors := []grpc.UnaryServerInterceptor{
		interceptor.Logging(s.logger),
		interceptor.Recovery(),
	}
	interceptors = append(interceptors, options.Interceptors...)
	statsOpts := []otelgrpc.Option{}
	if options.TracerProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithTracerProvider(options.TracerProvider))
	}
	if options.MeterProvider != nil {
		statsOpts = append(statsOpts, otelgrpc.WithMeterProvider(options.MeterProvider))
	}
	grpcOpts := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(statsOpts...)),
		grpc.ChainUnaryInterceptor(
			interceptors...,
		),
	}

	s.server = grpc.NewServer(grpcOpts...)

	// Register health check service
	s.health = health.NewServer()
	grpc_health_v1.RegisterHealthServer(s.server, s.health)
	return s
}

// Server 是 gRPC 服务组件，实现 lynx.ServerLike 接口。
type Server struct {
	server   *grpc.Server
	listener net.Listener
	logger   *slog.Logger
	o        Options
	health   *health.Server
	running  atomic.Bool
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
func (s *Server) Init(app lynx.App) error {
	return nil
}

// Start 启动 gRPC 服务并开始监听，阻塞至服务退出。
func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting gRPC server, listening on "+s.o.Addr)

	lis, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		return err
	}
	s.listener = lis

	// Set the server to healthy
	s.health.SetServingStatus("grpc", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register reflection service
	reflection.Register(s.server)

	s.running.Store(true)
	return s.server.Serve(lis)
}

// Stop 优雅关停 gRPC 服务：先关闭监听器，再等待在途请求完成，超时后强制停止。
func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping gRPC server")
	if s.health != nil {
		s.health.SetServingStatus("grpc", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	}
	s.running.Store(false)

	// Close the listener first to stop accepting new connections
	if s.listener != nil {
		_ = s.listener.Close()
	}

	if s.server == nil {
		return
	}

	// Fall back to the configured timeout when the caller's context has no deadline.
	if _, ok := ctx.Deadline(); !ok && s.o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.o.Timeout)
		defer cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.server.GracefulStop()
	}()

	select {
	case <-done:
		s.logger.Info("gRPC server stopped gracefully")
	case <-ctx.Done():
		s.logger.Warn("graceful stop timeout, forcing stop")
		s.server.Stop()
		<-done
	}
}

// GetServer 返回底层 *grpc.Server 实例，用于注册业务服务实现。
func (s *Server) GetServer() *grpc.Server {
	return s.server
}

var _ lynx.ServerLike = (*Server)(nil)
