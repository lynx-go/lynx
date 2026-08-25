// Package grpc 提供 gRPC 服务器服务，内置日志/恢复拦截器、
// 健康检查与反射服务，支持 OpenTelemetry 插装。
package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
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
	// DefaultHealthCheckTimeout 是健康轮询中单个 checker 的执行上限
	//（SC-03）：CheckHealth 接口无 ctx 参数，挂死的 checker 会卡死轮询
	// goroutine 并把状态冻结在 SERVING。
	DefaultHealthCheckTimeout = 3 * time.Second
)

// Options 是 gRPC 服务服务的配置项。
type Options struct {
	Addr string
	// AdvertiseAddr 是服务对外宣告的地址（host:port），由
	// WithAdvertiseAddr 设置，仅原样保存该字符串；为空表示未显式指定。
	AdvertiseAddr string
	// Timeout 是优雅关停的上限（历史名——gRPC 侧语义是关停而非读写
	// 超时，见 WithShutdownTimeout 别名，SC-05）；0 表示无上界。
	Timeout            time.Duration
	Logger             *slog.Logger
	Interceptors       []grpc.UnaryServerInterceptor
	StreamInterceptors []grpc.StreamServerInterceptor
	ServerOptions      []grpc.ServerOption
	TracerProvider     trace.TracerProvider
	MeterProvider      metric.MeterProvider
	// HealthCheck 提供 app 级健康检查器；非 nil 时按 HealthCheckPeriod
	// 轮询并同步到 grpc health 服务（依赖服务不健康时探测返回 NOT_SERVING）。
	HealthCheck lynx.HealthCheckersFunc
	// HealthCheckPeriod 是健康轮询间隔。
	HealthCheckPeriod time.Duration
	// HealthCheckTimeout 是轮询中单个 checker 的执行上限（缺省 3s，
	// SC-03），超时视为不健康；0 表示不限时（挂死 checker 会卡死轮询）。
	HealthCheckTimeout time.Duration
	// RequestLog 控制内置日志拦截器（缺省 true 保持历史行为，SC-07）：
	// 与 HTTP 侧默认关闭、Debug 级相反，gRPC 侧默认每 RPC 两条 Info，
	// 高吞吐服务建议 WithRequestLog(false) 或降级。
	RequestLog bool
	// RequestLogLevel 是内置日志拦截器的输出级别（缺省 Info，SC-07）。
	RequestLogLevel slog.Level
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
//
// 历史命名警示（SC-05）：gRPC 侧的 Timeout 语义是"优雅关停上限"，
// 与 HTTP 侧 WithTimeout 的"读写超时"完全不同——这是同名选项双语义
// 的已知坑，新代码请改用 WithShutdownTimeout 别名。
func WithTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.Timeout = timeout
	}
}

// WithShutdownTimeout 设置 gRPC 服务优雅关停的超时时间，与 WithTimeout
// 等价（内部同一字段）——新增别名与 HTTP 侧 WithShutdownTimeout 对齐
// （SC-05）。注意与 HTTP 侧 WithTimeout（读写超时）的语义差异：
// 传 0 表示无上界。
func WithShutdownTimeout(timeout time.Duration) Option {
	return WithTimeout(timeout)
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

// WithHealthCheckTimeout 设置健康轮询中单个 checker 的执行上限
// （缺省 3s，SC-03）：CheckHealth 接口无 ctx/超时（API 冻结），挂死的
// checker 在超时后被视为不健康，轮询 goroutine 不被卡死。传 0 表示
// 不限时（慎用）。
func WithHealthCheckTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.HealthCheckTimeout = timeout
	}
}

// WithRequestLog 控制内置 gRPC 请求日志拦截器（缺省 true 保持兼容，
// SC-07）：false 时每 RPC 不再产生两条日志。与 HTTP 侧差异：HTTP 的
// 请求日志默认关闭且为 Debug 级，gRPC 历史行为默认 Info 级全开——
// 高吞吐服务建议关闭或 WithRequestLogLevel(slog.LevelDebug) 降噪。
func WithRequestLog(enabled bool) Option {
	return func(o *Options) {
		o.RequestLog = enabled
	}
}

// WithRequestLogLevel 设置内置日志拦截器的输出级别（缺省 Info，SC-07）。
func WithRequestLogLevel(level slog.Level) Option {
	return func(o *Options) {
		o.RequestLogLevel = level
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
		Addr:               DefaultGRPCAddr,
		Timeout:            DefaultTimeout,
		Logger:             slog.Default(),
		RequestLog:         true,
		RequestLogLevel:    slog.LevelInfo,
		HealthCheckTimeout: DefaultHealthCheckTimeout,
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
	// Recovery 在最外层：链内任意一环（含用户拦截器）panic 都能被恢复，
	// 恢复时记录 panic 值 + 调用栈并返回通用错误（SC-04/SC-06）。
	// Bus 注入在请求时读取 s.bus（Init 后可用），供 Topic API 经 Context 解析。
	unaryInterceptors := []grpc.UnaryServerInterceptor{
		interceptor.RecoveryWithLogger(options.Logger),
		s.injectBusUnary(),
	}
	if options.RequestLog {
		unaryInterceptors = append(unaryInterceptors,
			interceptor.LoggingWithLevel(options.Logger, options.RequestLogLevel))
	}
	unaryInterceptors = append(unaryInterceptors, options.Interceptors...)
	// 流式 RPC 同样需要日志与 panic 恢复：gRPC 对流式 handler 的 panic
	// 没有内置保护，不加拦截器会直接崩溃整个进程。
	streamInterceptors := []grpc.StreamServerInterceptor{
		interceptor.RecoveryStreamWithLogger(options.Logger),
		s.injectBusStream(),
	}
	if options.RequestLog {
		streamInterceptors = append(streamInterceptors,
			interceptor.LoggingStreamWithLevel(options.Logger, options.RequestLogLevel))
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
	// 不会泄漏无人取消的轮询 goroutine。Init 会复位本标志（SC-15，
	// 重启语义留口；当前不支持同一实例不重 Init 的 restart）。
	stopped bool
	// stopRequested 标记 Stop 已被调用（原子，SC-02）：Start 在 Serve
	// 返回错误时据此把关停引起的 "use of closed network connection"
	// 类错误归一化为 nil，避免框架把正常关停发布为
	// lynx.service.failed 虚假事件。
	stopRequested atomic.Bool
	// started 守卫 Start 重入（SC-14）：二次 Start 会覆盖 listener
	// 造成泄漏，直接报错；Init 复位。
	started   atomic.Bool
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

// Init 初始化服务：接管 Bus，并复位生命周期标志（SC-15：lynx 生命周期
// 为 Init→Start→Stop，Init 复位 started/stopped/stopRequested 使
// "重新 Init 后可再 Start" 留有语义口子；当前不支持同实例不重 Init 的
// restart）。
func (s *Server) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		s.bus = ctx.Bus()
	}
	s.started.Store(false)
	s.stopRequested.Store(false)
	s.mu.Lock()
	s.stopped = false
	s.mu.Unlock()
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
// 正常关停（Stop 先关 listener）时 Serve 返回 "use of closed network
// connection" 类错误——stopRequested 已置位则归一化为 nil（SC-02），
// 避免框架把正常关停发布为 lynx.service.failed 虚假事件。
// 二次调用 Start 返回错误（SC-14）。
func (s *Server) Start(ctx context.Context) error {
	// 重入守卫：二次 Start 会覆盖 listener 并泄漏旧 listener。
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("grpc server: Start called more than once")
	}

	lis, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		// Listen 失败不算已启动：复位守卫，允许换地址重试。
		s.started.Store(false)
		return err
	}
	// 监听就绪后才打印 listening 日志（SC-16）：提前打印会在 Listen 失败
	//（如端口占用）时留下误导性的"正在监听"记录，且与 listening 事件
	// 语义对齐。
	s.logger.InfoContext(ctx, "starting gRPC server, listening on "+lis.Addr().String())
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
	serveErr := s.server.Serve(lis)
	// 正常关停：Stop 先关 listener 再 GracefulStop，Accept 因 listener 已
	// 关闭而失败——这是关停流程的一部分而非启动失败，归一化为 nil
	//（SC-02）。仅在 Stop 已请求时归一化，真实监听错误仍然上报。
	if serveErr != nil && s.stopRequested.Load() && isClosedConnError(serveErr) {
		return nil
	}
	return serveErr
}

// isClosedConnError 判定错误是否为"连接/监听器已被主动关闭"类：net 包
// 标准错误经 errors.Is 匹配，字符串兜底覆盖 grpc 内部未包装的路径
// （如 Windows 上的 Accept 错误文案）。
func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
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
	// 经限时并发 helper 执行（SC-03）：挂死的 checker 按超时不健康，
	// 轮询 goroutine 不被卡死、状态不会冻结在 SERVING。
	if err := runHealthChecks(s.o.HealthCheck, s.o.HealthCheckTimeout); err != nil {
		status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
	}
	s.health.SetServingStatus("", status)
	s.health.SetServingStatus("grpc", status)
}

// runHealthChecks 并发执行 checkers 并整体限时 timeout：任一失败立即
// 返回其错误；超时返回超时错误（视为不健康，SC-03，与 server/http 侧
// 同款实现）。lynx.Checker 接口无 ctx 参数（API 冻结），挂死的 checker
// 无法被打断只能被"放弃等待"——结果 channel 带缓冲，最终返回的
// checker goroutine 写入后自行退出，不卡轮询。固有边界（放弃等待模式
// 的残余代价）：永不返回的 checker（死锁类）其 goroutine 无法回收，
// 会随每次健康轮询累积泄漏，只能修复 checker 本身或重启进程消除。
// timeout <= 0 表示不限时：退化为顺序执行（保持旧行为的逃生口）。
func runHealthChecks(checkers lynx.HealthCheckersFunc, timeout time.Duration) error {
	cs := checkers()
	if len(cs) == 0 {
		return nil
	}
	if timeout <= 0 {
		for _, c := range cs {
			if err := checkOne(c); err != nil {
				return err
			}
		}
		return nil
	}
	results := make(chan error, len(cs))
	for _, c := range cs {
		go func(c lynx.Checker) {
			results <- checkOne(c)
		}(c)
	}
	// checker 并发起步，共享一个计时窗口即等效"每个 checker 限时"。
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for i := 0; i < len(cs); i++ {
		select {
		case err := <-results:
			if err != nil {
				return err
			}
		case <-timer.C:
			return fmt.Errorf("health check timed out after %s", timeout)
		}
	}
	return nil
}

// checkOne 执行单个 checker 并兜底其 panic：并发路径下 checker 运行在
// 独立 goroutine，panic 会直接崩溃进程，必须就地 recover 并按不健康
// 处理（与 server/http 侧同款）。
func checkOne(c lynx.Checker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("health check panicked: %v", r)
		}
	}()
	return c.CheckHealth()
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
	// 关 listener 之前置位（SC-02）：Start 侧据此把 Serve 返回的
	// closed-connection 错误归一化为 nil（正常关停不是失败）。
	s.stopRequested.Store(true)
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
