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
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

type Options struct {
	Addr         string
	Timeout      time.Duration
	Logger       *slog.Logger
	Interceptors []grpc.UnaryServerInterceptor
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

func NewServer(opts ...Option) *Server {
	options := Options{
		Addr:    ":9090",
		Timeout: time.Second * 60,
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
	grpcOpts := []grpc.ServerOption{
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
	server  *grpc.Server
	logger  *slog.Logger
	o       Options
	health  *health.Server
	running atomic.Bool
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
	if s.server == nil {
		return
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
