package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// TestReflectionRegisteredAtNewServer 回归：反射服务在 NewServer 时即注册
// （P0-2），而不是留到 Start 中（Serve 之后注册服务会 panic）。
func TestReflectionRegisteredAtNewServer(t *testing.T) {
	s := NewServer()
	info := s.server.GetServiceInfo()
	for _, name := range []string{"grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection"} {
		if _, ok := info[name]; !ok {
			t.Errorf("reflection service %q not registered after NewServer", name)
		}
	}
}

func TestNewServerDefaults(t *testing.T) {
	s := NewServer()
	if s.o.Addr != DefaultGRPCAddr {
		t.Errorf("Addr = %q, want %q", s.o.Addr, DefaultGRPCAddr)
	}
	if s.o.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", s.o.Timeout, DefaultTimeout)
	}
	if s.o.Logger == nil {
		t.Error("Logger should not be nil")
	}
	if s.GetServer() == nil {
		t.Error("GetServer() should not be nil")
	}
}

func TestNewServerOptions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	interceptor := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(ctx, req)
	}

	s := NewServer(
		WithAddr("127.0.0.1:19090"),
		WithTimeout(5*time.Second),
		WithLogger(logger),
		WithInterceptors(interceptor),
	)
	if s.o.Addr != "127.0.0.1:19090" {
		t.Errorf("Addr = %q, want %q", s.o.Addr, "127.0.0.1:19090")
	}
	if s.o.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want %v", s.o.Timeout, 5*time.Second)
	}
	if s.o.Logger != logger {
		t.Error("Logger was not set via WithLogger")
	}
	if len(s.o.Interceptors) != 1 {
		t.Errorf("len(Interceptors) = %d, want 1", len(s.o.Interceptors))
	}
}

func TestServerName(t *testing.T) {
	if got := NewServer().Name(); got != "grpc" {
		t.Errorf("Name() = %q, want %q", got, "grpc")
	}
}

func TestServerInit(t *testing.T) {
	if err := NewServer().Init(nil); err != nil {
		t.Errorf("Init() error = %v, want nil", err)
	}
}

func TestCheckHealthNotRunning(t *testing.T) {
	s := NewServer()
	if err := s.CheckHealth(); !errors.Is(err, grpc.ErrServerStopped) {
		t.Errorf("CheckHealth() error = %v, want %v", err, grpc.ErrServerStopped)
	}
}

// freeAddr reserves a free TCP address on localhost.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return addr
}

// waitRunning polls the race-free CheckHealth until the server is serving.
func waitRunning(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.CheckHealth() == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not start serving")
}

func TestStartStop(t *testing.T) {
	s := NewServer(WithAddr(freeAddr(t)))

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.Start(context.Background())
	}()
	waitRunning(t, s)

	_ = s.Stop(context.Background())

	select {
	case err := <-startErr:
		// Stop closes the listener before GracefulStop, so Serve returns a
		// "use of closed network connection" error.
		if err != nil && !strings.Contains(err.Error(), "closed network connection") {
			t.Errorf("Start() error = %v, want nil or closed-connection error after stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}

	if err := s.CheckHealth(); !errors.Is(err, grpc.ErrServerStopped) {
		t.Errorf("CheckHealth() error = %v after stop, want %v", err, grpc.ErrServerStopped)
	}
}

// rawCodec avoids pulling in protobuf for the test service.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error)      { return []byte{}, nil }
func (rawCodec) Unmarshal(data []byte, v any) error { return nil }
func (rawCodec) Name() string                       { return "raw" }

func init() {
	encoding.RegisterCodec(rawCodec{})
}

// blockerService is the handler type for the blocking test service.
type blockerService interface{}

// blocker blocks inside its RPC handler until the RPC context is cancelled.
type blocker struct {
	entered chan struct{}
}

func (b *blocker) handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStopForcesStopAfterTimeout is a regression test for commit 31e1db2: when
// the caller's context has no deadline, Stop must fall back to the configured
// Timeout and force-stop the server instead of hanging in GracefulStop.
func TestStopForcesStopAfterTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	addr := freeAddr(t)
	s := NewServer(WithAddr(addr), WithTimeout(timeout))

	b := &blocker{entered: make(chan struct{})}
	s.GetServer().RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Blocker",
		HandlerType: (*blockerService)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Block", Handler: b.handler},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "test.proto",
	}, b)

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.Start(context.Background())
	}()
	waitRunning(t, s)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	go func() {
		// The call is expected to fail when the server force-stops; ignore the error.
		_ = conn.Invoke(context.Background(), "/test.Blocker/Block", &struct{}{}, &struct{}{}, grpc.ForceCodec(rawCodec{}))
	}()

	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking handler was not entered")
	}

	start := time.Now()
	_ = s.Stop(context.Background()) // no deadline: Timeout fallback must kick in
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("Stop() took %v with an in-flight RPC, want roughly %v (Timeout fallback not applied)", elapsed, timeout)
	}

	select {
	case <-startErr:
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}

func TestServerTracingProducesSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	addr := freeAddr(t)
	s := NewServer(WithAddr(addr), WithTracerProvider(tp))
	s.server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Ping",
		HandlerType: (*interface{})(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Call",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				m := new(struct{})
				if err := dec(m); err != nil {
					return nil, err
				}
				return m, nil
			},
		}},
	}, struct{}{})

	go func() { _ = s.Start(context.Background()) }()
	waitRunning(t, s)
	// Failure-path cleanup; the explicit Stop below is the normal path.
	defer func() { _ = s.Stop(context.Background()) }()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Invoke(context.Background(), "/test.Ping/Call", &struct{}{}, &struct{}{}, grpc.ForceCodec(rawCodec{})); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	_ = s.Stop(context.Background())

	spans := recorder.Ended()
	var found bool
	for _, sp := range spans {
		if strings.Contains(sp.Name(), "test.Ping/Call") {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(spans))
		for _, sp := range spans {
			names = append(names, sp.Name())
		}
		t.Errorf("expected a span for test.Ping/Call, got spans: %v", names)
	}
}

// TestServerMetricsProduced verifies the WithMeterProvider wiring: with a
// MeterProvider passed in, the server's stats handler emits RPC metrics.
func TestServerMetricsProduced(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	addr := freeAddr(t)
	s := NewServer(WithAddr(addr), WithMeterProvider(mp))
	s.server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Ping",
		HandlerType: (*interface{})(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Call",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				m := new(struct{})
				if err := dec(m); err != nil {
					return nil, err
				}
				return m, nil
			},
		}},
	}, struct{}{})

	go func() { _ = s.Start(context.Background()) }()
	waitRunning(t, s)
	defer func() { _ = s.Stop(context.Background()) }()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Invoke(context.Background(), "/test.Ping/Call", &struct{}{}, &struct{}{}, grpc.ForceCodec(rawCodec{})); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	var md metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &md); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(md.ScopeMetrics) == 0 {
		t.Error("expected metrics from the RPC, got none")
	}
}

// flipChecker 是可切换健康状态的 checker，用于验证 health 轮询同步。
type flipChecker struct {
	healthy atomic.Bool
}

func (c *flipChecker) CheckHealth() error {
	if !c.healthy.Load() {
		return errors.New("dependency down")
	}
	return nil
}

// TestHealthStatusFollowsCheckers 回归：app 级健康检查器轮询结果同步到
// grpc health 服务，依赖不健康时探测返回 NOT_SERVING。
func TestHealthStatusFollowsCheckers(t *testing.T) {
	checker := &flipChecker{}
	checker.healthy.Store(true)
	s := NewServer(
		WithAddr("127.0.0.1:0"),
		WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{checker} }),
		WithHealthCheckPeriod(20*time.Millisecond),
	)
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	defer func() {
		_ = s.Stop(context.Background())
		select {
		case <-startErr:
		case <-time.After(5 * time.Second):
			t.Error("Start did not return after Stop")
		}
	}()

	var addr string
	for i := 0; i < 200 && addr == ""; i++ {
		s.mu.Lock()
		if s.listener != nil {
			addr = s.listener.Addr().String()
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server did not start listening")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()
	client := grpc_health_v1.NewHealthClient(conn)

	resp, err := client.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("initial status = %v, want SERVING", resp.Status)
	}

	// 依赖变不健康：轮询周期内应翻转为 NOT_SERVING。
	checker.healthy.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = client.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if resp.Status == grpc_health_v1.HealthCheckResponse_NOT_SERVING {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("health status did not flip to NOT_SERVING after checker failed")
}

// TestHealthCheckDefaultPeriodNotPanic 回归：WithHealthCheckers 未设置
// Period 时，NewServer 必须回退 DefaultHealthCheckPeriod，轮询 goroutine
// 不得用零值 Ticker 触发 time.NewTicker(0) panic。
func TestHealthCheckDefaultPeriodNotPanic(t *testing.T) {
	checker := &flipChecker{}
	checker.healthy.Store(true)
	s := NewServer(
		WithAddr(freeAddr(t)),
		WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{checker} }),
	)
	if s.o.HealthCheckPeriod != DefaultHealthCheckPeriod {
		t.Fatalf("HealthCheckPeriod = %v, want default %v", s.o.HealthCheckPeriod, DefaultHealthCheckPeriod)
	}
	// 负值同样回退缺省值。
	neg := NewServer(WithHealthCheckPeriod(-1 * time.Second))
	if neg.o.HealthCheckPeriod != DefaultHealthCheckPeriod {
		t.Fatalf("negative HealthCheckPeriod = %v, want default %v", neg.o.HealthCheckPeriod, DefaultHealthCheckPeriod)
	}

	// 走一遍真实 Start/Stop：健康轮询 goroutine 启动且不 panic。
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	waitRunning(t, s)
	_ = s.Stop(context.Background())
	select {
	case <-startErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestHealthPollerNotLeakedWhenStoppedBeforeStart 回归：Stop 早于 Start
// 执行到 startHealthPoller 时（healthCancel 为 nil），poller 启动必须在
// 同锁段内发现 stopped 并取消自身——不得留下永不取消的轮询 goroutine。
// 直接调用 startHealthPoller 是确定性的：修复前 healthCancel 非 nil
//（goroutine 已启动），修复后保持 nil。
func TestHealthPollerNotLeakedWhenStoppedBeforeStart(t *testing.T) {
	checker := &flipChecker{}
	checker.healthy.Store(true)
	s := NewServer(
		WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{checker} }),
		WithHealthCheckPeriod(20*time.Millisecond),
	)
	// Stop 先于 Start 执行到 poller 启动点：healthCancel 尚无值。
	_ = s.Stop(context.Background())

	before := runtime.NumGoroutine()
	s.startHealthPoller()
	time.Sleep(60 * time.Millisecond) // 给足 goroutine 启动与首个 tick

	s.mu.Lock()
	cancel := s.healthCancel
	s.mu.Unlock()
	if cancel != nil {
		t.Fatal("health poller started after Stop-before-Start (leaked goroutine)")
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutine count %d -> %d, health poller leaked", before, after)
	}
}

// TestWithStreamInterceptors 验证流式拦截器选项追加生效。
func TestWithStreamInterceptors(t *testing.T) {
	var called atomic.Int32
	streamInt := func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		called.Add(1)
		return handler(srv, ss)
	}
	s := NewServer(WithStreamInterceptors(streamInt))
	if len(s.o.StreamInterceptors) != 1 {
		t.Fatalf("len(StreamInterceptors) = %d, want 1", len(s.o.StreamInterceptors))
	}
	if called.Load() != 0 {
		t.Fatal("interceptor should not run before serving")
	}
}
