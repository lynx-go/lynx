package http

import (
	"context"
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
	DefaultHTTPAddr = ":8080"
	DefaultTimeout  = 60 * time.Second
)

func NewRouter() *http.ServeMux {
	return http.NewServeMux()
}

type Options struct {
	Addr           string
	Timeout        time.Duration
	HealthCheck    lynx.HealthCheckFunc
	Logger         *slog.Logger
	RequestLog     bool
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
	Middlewares    []Middleware
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
	// mu guards the embedded *server.Server, which is assigned in Start and
	// read in Stop; the two may run on different goroutines during shutdown.
	mu      sync.RWMutex
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
	driver := server.NewDefaultDriver()
	if s.o.Timeout > 0 {
		// Apply the configured timeout to the underlying http.Server read/write timeouts.
		driver.Server.ReadHeaderTimeout = s.o.Timeout
		driver.Server.ReadTimeout = s.o.Timeout
		driver.Server.WriteTimeout = s.o.Timeout
	}
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

func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping HTTP server")
	s.mu.RLock()
	srv := s.Server
	s.mu.RUnlock()
	if srv == nil {
		return
	}
	if err := srv.Shutdown(ctx); err != nil {
		log.ErrorContext(ctx, "failed to shutting down http server", err)
	}
}

var _ lynx.Component = (*Server)(nil)
