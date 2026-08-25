// Package grpc 提供 gRPC 服务器服务，内置日志/恢复拦截器、
// 健康检查与反射服务，支持 OpenTelemetry 插装。
package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/server/grpc/interceptor"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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

// Options 是 gRPC 服务服务的配置项。
type Options struct {
	Addr string
	// AdvertiseAddr 是服务对外宣告的地址（host:port），由
	// WithAdvertiseAddr 设置，仅原样保存该字符串；为空表示未显式指定。
	AdvertiseAddr      string
	Timeout            time.Duration
	Logger             *slog.Logger
	Interceptors       []grpc.UnaryServerInterceptor
	StreamInterceptors []grpc.StreamServerInterceptor
	ServerOptions      []grpc.ServerOption
	TracerProvider     trace.TracerProvider
	MeterProvider      metric.MeterProvider
	// HealthCheck 提供 app 级健康检查器；非 nil 时按 HealthCheckPeriod
	// 轮询并同步到 grpc health 服务（依赖服务不健康时探测返回 NOT_SERVING）。
	HealthCheck       lynx.HealthCheckersFunc
	HealthCheckPeriod time.Duration
	// TLSConfig 非 nil 时启用 TLS 传输（credentials.NewTLS），与 HTTP 侧
	// WithTLSConfig 语义对齐。
	TLSConfig *tls.Config
}

// Option 用于配置 gRPC 服务 Options 的选项函数。
type Option func(*Options)

// WithAddr 设置 gRPC 服务监听地址。
func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

// WithAdvertiseAddr 设置 gRPC 服务对外宣告的地址（host:port），仅保存该
// 字符串，供注册发现等场景经 AdvertiseAddr 读取；不影响实际监听地址，
// 也不参与协议推断。
func WithAdvertiseAddr(hostPort string) Option {
	return func(o *Options) {
		o.AdvertiseAddr = hostPort
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

// WithHealthCheckers 设置 app 级健康检查函数，轮询结果同步到 grpc health
// 服务（与 HTTP 侧 WithHealthCheckers 命名对齐）。
func WithHealthCheckers(hc lynx.HealthCheckersFunc) Option {
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

// WithTLSConfig 启用 TLS 传输，cfg 须已装配证书（tls.LoadX509KeyPair 等）。
// 与 WithServerOptions(grpc.Creds(...)) 同时使用时 TLSConfig 优先：grpc 对
// 重复 Creds 取最后应用者（实测确认），TLSConfig 的 Creds 装配在 ServerOptions
// 之后，因此覆盖后者。两者同传属误用，仅以 TLSConfig 为准。
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *Options) {
		o.TLSConfig = cfg
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

// NewServer 创建 gRPC 服务服务，内置日志、恢复拦截器与 OpenTelemetry stats handler，
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
		ready:  make(chan struct{}),
	}
	// Recovery 在最外层：链内任意一环（含用户拦截器）panic 都能被恢复。
	// Bus 注入在请求时读取 s.bus（Init 后可用），供 Topic API 经 Context 解析。
	unaryInterceptors := []grpc.UnaryServerInterceptor{
		interceptor.Recovery(),
		s.injectBusUnary(),
		interceptor.Logging(s.logger),
	}
	unaryInterceptors = append(unaryInterceptors, options.Interceptors...)
	// 流式 RPC 同样需要日志与 panic 恢复：gRPC 对流式 handler 的 panic
	// 没有内置保护，不加拦截器会直接崩溃整个进程。
	streamInterceptors := []grpc.StreamServerInterceptor{
		interceptor.RecoveryStream(),
		s.injectBusStream(),
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
	// TLSConfig 非 nil 时启用 TLS 传输。装配在 ServerOptions 之后：
	// grpc 对重复 Creds 取最后应用者，保证 TLSConfig 优先（见 WithTLSConfig）。
	if options.TLSConfig != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(options.TLSConfig)))
	}

	s.server = grpc.NewServer(grpcOpts...)

	// Register health check service
	s.health = health.NewServer()
	grpc_health_v1.RegisterHealthServer(s.server, s.health)
	// Register reflection service：注册必须在 Serve 之前完成（Serve 后
	// 注册服务会 panic）；放在 NewServer 中保证一次创建即注册，Start 可
	// 安全重入。
	reflection.Register(s.server)
	return s
}

// Server 是 gRPC 服务，实现 lynx.Service 接口。
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
	stopped   bool
	running   atomic.Bool
	bus       eventbus.Bus
	ready     chan struct{}
	readyOnce sync.Once
}

// CheckHealth 实现健康检查，服务未处于运行状态时返回错误。
func (s *Server) CheckHealth() error {
	if !s.running.Load() {
		return grpc.ErrServerStopped
	}
	// Check if the server is still serving
	return nil
}

// Name 返回服务名称 "grpc"。
func (s *Server) Name() string {
	return "grpc"
}

// Init 初始化服务，gRPC 服务无需在初始化阶段做额外工作。
func (s *Server) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		s.bus = ctx.Bus()
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.publishEvent(eventbus.TopicGRPCListening, eventbus.ServerEvent{Service: "grpc", Addr: lis.Addr().String(), AdvertiseAddr: s.o.AdvertiseAddr, Time: time.Now()})
	s.closeReady()

	// Set the server to healthy, for both the named and the standard empty
	// service name used by most gRPC health probes.
	s.health.SetServingStatus("grpc", grpc_health_v1.HealthCheckResponse_SERVING)
	s.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	s.running.Store(true)
	// Serve 返回（监听失败/优雅停止）后复位运行标志，CheckHealth 不再报健康。
	defer s.running.Store(false)

	if s.o.HealthCheck != nil {
		s.startHealthPoller()
	}
	return s.server.Serve(lis)
}

// startHealthPoller 按 HealthCheckPeriod 轮询 app 级健康检查器并同步到
// grpc health 服务：任一依赖服务不健康时探测返回 NOT_SERVING。
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
	s.publishEvent(eventbus.TopicGRPCStopping, eventbus.ServerEvent{Service: "grpc", Addr: s.Addr(), AdvertiseAddr: s.o.AdvertiseAddr, Time: time.Now()})
	defer s.publishEvent(eventbus.TopicGRPCStopped, eventbus.ServerEvent{Service: "grpc", Time: time.Now()})
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
		if err := lis.Close(); err != nil {
			s.logger.ErrorContext(ctx, "error closing gRPC listener", "error", err)
		}
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

// injectBusUnary 将 Bus 写入 unary 请求 Context（Init 后 s.bus 可用）。
func (s *Server) injectBusUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if s.bus != nil {
			ctx = eventbus.ContextWithBus(ctx, s.bus)
		}
		return handler(ctx, req)
	}
}

type busStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s busStream) Context() context.Context { return s.ctx }

// injectBusStream 将 Bus 写入 stream 请求 Context。
func (s *Server) injectBusStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		if s.bus != nil {
			ctx = eventbus.ContextWithBus(ctx, s.bus)
		}
		return handler(srv, busStream{ServerStream: ss, ctx: ctx})
	}
}

var _ lynx.Service = (*Server)(nil)

var _ lynx.Checker = (*Server)(nil)

var _ lynx.Ready = (*Server)(nil)
