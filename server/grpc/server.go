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

type Options struct {
	Addr           string
	Timeout        time.Duration
	Logger         *slog.Logger
	Interceptors   []grpc.UnaryServerInterceptor
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

type Option func(*Options)

func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

func WithTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.Timeout = timeout
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

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

type Server struct {
	server   *grpc.Server
	listener net.Listener
	logger   *slog.Logger
	o        Options
	health   *health.Server
	running  atomic.Bool
}

func (s *Server) CheckHealth() error {
	if !s.running.Load() {
		return grpc.ErrServerStopped
	}
	// Check if the server is still serving
	return nil
}

func (s *Server) Name() string {
	return "grpc"
}

func (s *Server) Init(app lynx.Lynx) error {
	return nil
}

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

func (s *Server) GetServer() *grpc.Server {
	return s.server
}

var _ lynx.ServerLike = (*Server)(nil)
