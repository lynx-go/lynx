package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"gocloud.dev/server"
	"gocloud.dev/server/health"
)

// Default values for HTTP server configuration.
const (
	DefaultHTTPAddr = ":8080"
	DefaultTimeout  = 60 * time.Second
)

func NewRouter() *http.ServeMux {
	return http.NewServeMux()
}

type Options struct {
	Addr        string
	Timeout     time.Duration
	HealthCheck lynx.HealthCheckFunc
	Logger      *slog.Logger
	RequestLog  bool
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

func WithHealthCheck(hc lynx.HealthCheckFunc) Option {
	return func(o *Options) {
		o.HealthCheck = hc
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

func WithRequestLog(requestLog bool) Option {
	return func(o *Options) {
		o.RequestLog = requestLog
	}
}

func NewServer(handler http.Handler, opts ...Option) *Server {
	options := Options{
		Addr:    DefaultHTTPAddr,
		Timeout: DefaultTimeout,
		Logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(&options)
	}

	return &Server{
		logger:  options.Logger,
		o:       options,
		handler: handler,
	}
}

type Server struct {
	*server.Server
	logger  *slog.Logger
	o       Options
	handler http.Handler
}

func (s *Server) Name() string {
	return "http"
}

func (s *Server) Init(app lynx.Lynx) error {
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting HTTP server, listening on "+s.o.Addr)
	var healthChecks []health.Checker
	if s.o.HealthCheck != nil {
		healthChecks = s.o.HealthCheck()
	}
	opts := &server.Options{
		HealthChecks: healthChecks,
		//TraceTextMapPropagator: sdserver.NewTextMapPropagator(),
		Driver: server.NewDefaultDriver(),
	}
	if s.o.RequestLog {
		opts.RequestLogger = NewRequestLogger(s.logger, func(err error) {
			log.ErrorContext(ctx, "failed to log HTTP request", err)
		})
	}

	hs := server.New(s.handler, opts)
	s.Server = hs
	return s.Server.ListenAndServe(s.o.Addr)
}

func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping HTTP server")
	if err := s.Server.Shutdown(ctx); err != nil {
		log.ErrorContext(ctx, "failed to shutting down http server", err)
	}
}

var _ lynx.Component = (*Server)(nil)
