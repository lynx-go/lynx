package http

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNewServerDefaults(t *testing.T) {
	s := NewServer(http.NewServeMux())
	if s.o.Addr != DefaultHTTPAddr {
		t.Errorf("Addr = %q, want %q", s.o.Addr, DefaultHTTPAddr)
	}
	if s.o.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", s.o.Timeout, DefaultTimeout)
	}
	if s.o.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", s.o.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if s.o.Logger == nil {
		t.Error("Logger should not be nil")
	}
	if s.handler == nil {
		t.Error("handler should not be nil")
	}
}

func TestNewServerOptions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := NewServer(http.NewServeMux(),
		WithAddr("127.0.0.1:18080"),
		WithTimeout(5*time.Second),
		WithRequestLog(true),
		WithLogger(logger),
		WithHealthCheckers(func() []lynx.Checker { return nil }),
	)
	if s.o.Addr != "127.0.0.1:18080" {
		t.Errorf("Addr = %q, want %q", s.o.Addr, "127.0.0.1:18080")
	}
	if s.o.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want %v", s.o.Timeout, 5*time.Second)
	}
	if !s.o.RequestLog {
		t.Error("RequestLog = false, want true")
	}
	if s.o.Logger != logger {
		t.Error("Logger was not set via WithLogger")
	}
	if s.o.HealthCheckers == nil {
		t.Error("HealthCheckers was not set via WithHealthCheckers")
	}
}

func TestServerName(t *testing.T) {
	s := NewServer(http.NewServeMux())
	if got := s.Name(); got != "http" {
		t.Errorf("Name() = %q, want %q", got, "http")
	}
}

func TestServerInit(t *testing.T) {
	s := NewServer(http.NewServeMux())
	if err := s.Init(nil); err != nil {
		t.Errorf("Init() error = %v, want nil", err)
	}
}

func TestAppendLatency(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: 0, want: "0.000000000s"},
		{d: time.Second, want: "1.000000000s"},
		{d: 1500 * time.Millisecond, want: "1.500000000s"},
	}
	for _, tt := range tests {
		if got := string(appendLatency(nil, tt.d)); got != tt.want {
			t.Errorf("appendLatency(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestRequestLoggerLog(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	onErrCalled := false
	l := NewRequestLogger(logger, func(err error) { onErrCalled = true })

	req, err := http.NewRequest(http.MethodGet, "http://example.com/path", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	l.Log(&Entry{
		Request:      req,
		ReceivedTime: time.Now(),
		Status:       http.StatusOK,
		Latency:      10 * time.Millisecond,
	})
	if onErrCalled {
		t.Error("onErr should not be called for a valid entry")
	}
}

// TestTimeoutAppliedToServer is a regression test for commit 31e1db2: the
// Timeout option must be applied to the underlying http.Server read/write
// timeouts. Before the fix, an idle connection would never be closed.
func TestTimeoutAppliedToServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	const timeout = 300 * time.Millisecond
	handler := http.NewServeMux()
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := NewServer(handler, WithAddr(addr), WithTimeout(timeout))

	startErr := make(chan error, 1)
	go func() {
		startErr <- srv.Start(context.Background())
	}()

	// Wait until the server accepts connections.
	waitForDial(t, addr)

	// Open a connection and send nothing: ReadHeaderTimeout should make the
	// server close it shortly after `timeout`.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	start := time.Now()
	_, readErr := conn.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if readErr == nil {
		t.Error("Read() = nil error, want the server to close the idle connection")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Error("Read() hit the client-side deadline; server did not close the idle connection (Timeout option not applied)")
	}
	if elapsed > 3*time.Second {
		t.Errorf("connection closed after %v, want roughly %v", elapsed, timeout)
	}

	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}

func TestServerTracingProducesSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Grab a free port, then release it for the server to bind.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithAddr(addr), WithTracerProvider(tp))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)
	// Failure-path cleanup; the explicit Stop below is the normal path and
	// Shutdown is idempotent.
	defer func() { _ = srv.Stop(context.Background()) }()

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}

	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected at least one span from the HTTP request, got none")
	}
	var found bool
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindServer {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a server-kind span, got: %v", spans)
	}
}

// TestServerMetricsProduced verifies the WithMeterProvider wiring: with a
// MeterProvider passed in, the server's instrumentation emits metrics for
// handled requests.
func TestServerMetricsProduced(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithAddr(addr), WithMeterProvider(mp))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)
	defer func() { _ = srv.Stop(context.Background()) }()

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	var md metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &md); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(md.ScopeMetrics) == 0 {
		t.Error("expected metrics from the HTTP request, got none")
	}
}

// TestServerPropagatorExtractsTraceParent verifies the WithPropagator wiring:
// an incoming traceparent header is extracted and linked to the server span.
// (gocloud wraps the handler with otelhttp's public-endpoint option, so the
// remote context becomes a span link rather than a parent.)
func TestServerPropagatorExtractsTraceParent(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithAddr(addr), WithTracerProvider(tp), WithPropagator(propagation.TraceContext{}))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)
	defer func() { _ = srv.Stop(context.Background()) }()

	const traceID = "0102030405060708090a0b0c0d0e0f10"
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("traceparent", "00-"+traceID+"-1112131415161718-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}

	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected at least one span from the HTTP request, got none")
	}
	var linked bool
	for _, s := range spans {
		for _, link := range s.Links() {
			if link.SpanContext.TraceID().String() == traceID {
				linked = true
			}
		}
	}
	if !linked {
		t.Errorf("no span links to the incoming traceparent trace %s", traceID)
	}
}

func waitForDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not start accepting connections", addr)
}

var _ lynx.Service = (*Server)(nil)

// TestStopBeforeStartIsNoop ensures Stop is safe before Start has assigned
// the underlying server (e.g. shutdown during startup), and does not panic
// on the nil embedded server.
func TestStopBeforeStartIsNoop(t *testing.T) {
	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	_ = srv.Stop(context.Background())
}

// TestStopForcesCloseAfterTimeout is a regression test for the unbounded HTTP
// shutdown bug: a handler that never returns (long-poll/streaming) must not
// hang Stop indefinitely. After ShutdownTimeout, Stop force-closes the server.
func TestStopForcesCloseAfterTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	entered := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered: // already entered (second request)
		default:
			close(entered)
		}
		<-r.Context().Done() // block until the connection is closed
	})
	srv := NewServer(handler, WithAddr(addr), WithShutdownTimeout(300*time.Millisecond))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)

	go func() {
		_, _ = http.Get("http://" + addr + "/")
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not entered")
	}

	start := time.Now()
	_ = srv.Stop(context.Background())
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("Stop() took %v, want roughly ShutdownTimeout (force close)", elapsed)
	}

	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}

// TestStopReturnsTimeoutErrorOnShutdownDeadline is a regression test for the
// HTTP Stop timeout race (P0-1): when Shutdown hits its deadline, Stop must
// ALWAYS return a timeout error and force-close the server — it must never
// return nil with lingering connections. Stop selects between `done` and
// `ctx.Done()`, and either side may win per run; the test repeats the full
// lifecycle (with -race) so both branches are exercised.
func TestStopReturnsTimeoutErrorOnShutdownDeadline(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Listen() error = %v", err)
			}
			addr := l.Addr().String()
			_ = l.Close()

			entered := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-r.Context().Done() // never return; block until forced close
			})
			srv := NewServer(handler, WithAddr(addr), WithShutdownTimeout(50*time.Millisecond))

			startErr := make(chan error, 1)
			go func() { startErr <- srv.Start(context.Background()) }()
			waitForDial(t, addr)

			go func() {
				_, _ = http.Get("http://" + addr + "/")
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("handler was not entered")
			}

			stopErr := srv.Stop(context.Background())
			if stopErr == nil {
				t.Fatal("Stop() = nil, want timeout error (deadline must force-close and report)")
			}
			if !errors.Is(stopErr, context.DeadlineExceeded) {
				t.Errorf("Stop() error = %v, want context.DeadlineExceeded in chain", stopErr)
			}

			select {
			case err := <-startErr:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("Start() did not return after Stop()")
			}
		})
	}
}

// TestServerOptionsEscapeHatch 验证 WithServerOptions 透传底层 *http.Server。
func TestServerOptionsEscapeHatch(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	var got *http.Server
	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), WithAddr(addr), WithServerOptions(func(s *http.Server) {
		s.MaxHeaderBytes = 4096
		got = s
	}))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)

	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}
	if got == nil || got.MaxHeaderBytes != 4096 {
		t.Fatalf("ServerOptions not applied: got = %+v", got)
	}
}

// TestProvidersDoNotMutateGlobals 回归：显式注入的 otel provider 不得
// 改写进程全局 provider（旧 gocloud 实现通过 init 静默改写全局）。
func TestProvidersDoNotMutateGlobals(t *testing.T) {
	before := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithAddr(addr), WithTracerProvider(tp))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)
	defer func() { _ = srv.Stop(context.Background()) }()

	// 请求一次，确认插装确实使用了显式 provider。
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()
	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}

	if got := otel.GetTracerProvider(); got != before {
		t.Fatal("global tracer provider was mutated by server start")
	}
	if len(recorder.Ended()) == 0 {
		t.Fatal("explicit tracer provider was not used (no spans recorded)")
	}
}

func TestServerAddrBeforeStart(t *testing.T) {
	s := NewServer(http.NewServeMux())
	if got := s.Addr(); got != "" {
		t.Errorf("Addr() = %q before Start, want empty", got)
	}
}

// TestServerAddrAfterListen 验证 Start 用随机端口 ":0" 启动后 Addr()
// 返回实际分配的 host:port（语义同 debug.Service.Addr）。
func TestServerAddrAfterListen(t *testing.T) {
	srv := NewServer(http.NewServeMux(), WithAddr("127.0.0.1:0"))
	if got := srv.Addr(); got != "" {
		t.Errorf("Addr() = %q before Start, want empty", got)
	}

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()

	// 等待 listener 绑定完成。
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Addr() is still empty after Start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	addr := srv.Addr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("Addr() = %q is not a valid host:port: %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("Addr() host = %q, want %q", host, "127.0.0.1")
	}
	if port == "0" {
		t.Errorf("Addr() port = %q, want the actual allocated port", port)
	}

	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}

func TestWithAdvertiseAddr(t *testing.T) {
	s := NewServer(http.NewServeMux())
	if got := s.AdvertiseAddr(); got != "" {
		t.Errorf("AdvertiseAddr() = %q, want empty", got)
	}
	s = NewServer(http.NewServeMux(), WithAdvertiseAddr("10.0.0.1:8080"))
	if got := s.AdvertiseAddr(); got != "10.0.0.1:8080" {
		t.Errorf("AdvertiseAddr() = %q, want %q", got, "10.0.0.1:8080")
	}
}

func TestServerReadyAfterListen(t *testing.T) {
	srv := NewServer(http.NewServeMux(), WithAddr("127.0.0.1:0"))
	select {
	case <-srv.Ready():
		t.Fatal("Ready() closed before Start")
	default:
	}

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()

	select {
	case <-srv.Ready():
	case err := <-startErr:
		t.Fatalf("Start() returned before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Ready() did not close after Listen")
	}
	if srv.Addr() == "" {
		t.Error("Addr() empty after Ready closed")
	}

	_ = srv.Stop(context.Background())
	select {
	case <-startErr:
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}

func TestServerReadyNotClosedOnListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()

	srv := NewServer(http.NewServeMux(), WithAddr(ln.Addr().String()))
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("Start() = nil, want listen error")
	}
	select {
	case <-srv.Ready():
		t.Fatal("Ready() closed on Listen failure")
	default:
	}
}

// --- SC-02：正常关停后 Start 必须返回 nil（不发 failed 事件） ---

// TestStartReturnsNilAfterStop 回归 SC-02：Stop 后 Serve 返回的
// http.ErrServerClosed 必须归一化为 nil——否则框架每次 SIGTERM 都把
// 正常关停发布为 lynx.service.failed 虚假事件。
func TestStartReturnsNilAfterStop(t *testing.T) {
	srv := NewServer(http.NewServeMux(), WithAddr("127.0.0.1:0"))
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, srv.mustAddr(t))
	_ = srv.Stop(context.Background())

	select {
	case err := <-startErr:
		if err != nil {
			t.Errorf("Start() error = %v, want nil（正常关停不是失败, SC-02）", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}
}

// mustAddr 等待 Start 完成 listener 绑定并返回实际地址。
func (s *Server) mustAddr(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if addr := s.Addr(); addr != "" {
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- SC-14：Start 重入守卫 ---

// TestStartRejectsReentry 回归 SC-14：二次 Start 覆盖 httpServer/listener
// 会泄漏旧 listener，必须直接报错。
func TestStartRejectsReentry(t *testing.T) {
	srv := NewServer(http.NewServeMux(), WithAddr("127.0.0.1:0"))
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, srv.mustAddr(t))
	defer func() { _ = srv.Stop(context.Background()) }()

	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("second Start() = nil error, want reentry guard error (SC-14)")
	} else if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("second Start() error = %v, want reentry guard error", err)
	}
	// 首次 Start 仍在运行（未被守卫误伤）：短暂窗口内不得返回。
	select {
	case err := <-startErr:
		t.Fatalf("first Start() returned %v; want still running", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStartGuardResetsOnListenFailure：Listen 失败（如端口被占）不算已
// 启动，守卫复位后允许换址/重试。
func TestStartGuardResetsOnListenFailure(t *testing.T) {
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = occ.Close() }()
	addr := occ.Addr().String()

	srv := NewServer(http.NewServeMux(), WithAddr(addr))
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("Start on occupied port = nil, want listen error")
	}
	// 端口释放后重试：不应被守卫以 "more than once" 拒绝。
	_ = occ.Close()
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, srv.mustAddr(t))
	_ = srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil {
			t.Errorf("retry Start() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry Start() did not return after Stop()")
	}
}

// --- SC-05：Stop 的关停 deadline 取 min ---

// TestStopTakesMinOfCallerDeadlineAndShutdownTimeout 回归 SC-05：调用方
// ctx 带 10s deadline、ShutdownTimeout=200ms 时必须取较小者（~200ms 强制
// 关闭），而不是等满调用方 deadline。
func TestStopTakesMinOfCallerDeadlineAndShutdownTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	entered := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-r.Context().Done() // 阻塞到连接被强制关闭
	})
	srv := NewServer(handler, WithAddr(addr), WithShutdownTimeout(200*time.Millisecond))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)
	defer func() { _ = srv.Stop(context.Background()) }()

	go func() { _, _ = http.Get("http://" + addr + "/") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	stopErr := srv.Stop(ctx)
	elapsed := time.Since(start)

	if stopErr == nil {
		t.Fatal("Stop() = nil, want timeout error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("Stop() took %v, want ~200ms（min 语义未生效, SC-05）", elapsed)
	}
}

// --- SC-03/SC-08/SC-21：healthz 端点行为 ---

// stubChecker 是可配置结果的健康检查器。
type stubChecker struct {
	err   error
	block chan struct{} // 非 nil 时挂死直到 channel 关闭
}

func (c *stubChecker) CheckHealth() error {
	if c.block != nil {
		<-c.block
		return nil
	}
	return c.err
}

// startHTTPServerForTest 起真实 server 并等待可拨号，返回地址与收尾函数。
// 固定 127.0.0.1:0：NO_PROXY 环境通常只豁免回环地址，其他绑定地址会被
// 本地代理拦截导致 502。
func startHTTPServerForTest(t *testing.T, opts ...Option) (string, func()) {
	t.Helper()
	opts = append([]Option{WithAddr("127.0.0.1:0")}, opts...)
	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("user")) // 业务兜底路径标记
	}), opts...)
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	addr := srv.mustAddr(t)
	return addr, func() {
		_ = srv.Stop(context.Background())
		select {
		case <-startErr:
		case <-time.After(5 * time.Second):
			t.Error("Start did not return after Stop")
		}
	}
}

// healthzGet 带 3s 客户端超时请求端点（探测不得挂死时能及时失败），
// 返回状态码与响应体。
func healthzGet(t *testing.T, url string) (int, string, error) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), nil
}

// TestHealthzEndpoints 回归 SC-21：liveness 恒 200；readiness 按检查器
// 结果返回 200/503；空检查器列表与未配置检查器恒 200。
func TestHealthzEndpoints(t *testing.T) {
	ok := &stubChecker{}
	bad := &stubChecker{err: errors.New("dependency down")}

	cases := []struct {
		name    string
		opts    []Option
		path    string
		want    int
		wantSub string
	}{
		{
			name: "liveness always ok",
			path: "/healthz/liveness",
			want: http.StatusOK,
		},
		{
			name:    "readiness healthy",
			opts:    []Option{WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{ok} })},
			path:    "/healthz/readiness",
			want:    http.StatusOK,
			wantSub: "",
		},
		{
			name:    "readiness unhealthy",
			opts:    []Option{WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{bad} })},
			path:    "/healthz/readiness",
			want:    http.StatusServiceUnavailable,
			wantSub: "dependency down",
		},
		{
			name: "readiness empty checker slice",
			opts: []Option{WithHealthCheckers(func() []lynx.Checker { return nil })},
			path: "/healthz/readiness",
			want: http.StatusOK,
		},
		{
			name: "readiness no checkers configured",
			path: "/healthz/readiness",
			want: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, stop := startHTTPServerForTest(t, tc.opts...)
			defer stop()
			status, _, err := healthzGet(t, "http://"+addr+tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			if status != tc.want {
				t.Errorf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

// TestReadinessHungCheckerTimesOut 回归 SC-03：挂死的 checker（CheckHealth
// 永不返回）不得挂起探测请求——HealthCheckTimeout 到期后按 503 返回。
func TestReadinessHungCheckerTimesOut(t *testing.T) {
	hung := &stubChecker{block: make(chan struct{})}
	defer close(hung.block) // 测试结束后放行挂死的 checker goroutine

	addr, stop := startHTTPServerForTest(t,
		WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{hung} }),
		WithHealthCheckTimeout(100*time.Millisecond))
	defer stop()

	start := time.Now()
	status, _, err := healthzGet(t, "http://"+addr+"/healthz/readiness")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("readiness 探测请求挂死或失败（SC-03 回归）: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503（超时视为不健康）", status)
	}
	if elapsed > 2*time.Second {
		t.Errorf("探测耗时 %v，超时后应尽快返回", elapsed)
	}
}

// TestReadinessCheckerPanicRecovered 回归 SC-08：健康端点绕过用户中间件
// 链，checker panic 必须被兜底（按不健康 503 处理），不拖垮进程——
// 并发路径下 checker 运行在独立 goroutine，靠 runHealthChecks 内的
// recover 兜底；handler 自身的 panic 由外层 Recovery 兜底。
func TestReadinessCheckerPanicRecovered(t *testing.T) {
	addr, stop := startHTTPServerForTest(t,
		WithHealthCheckers(func() []lynx.Checker { return []lynx.Checker{&panicChecker{}} }))
	defer stop()

	status, _, err := healthzGet(t, "http://"+addr+"/healthz/readiness")
	if err != nil {
		t.Fatalf("GET readiness: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503（panic 的 checker 按不健康处理, SC-08）", status)
	}
	// 进程仍存活：liveness 继续可用。
	status2, _, err := healthzGet(t, "http://"+addr+"/healthz/liveness")
	if err != nil {
		t.Fatalf("GET liveness after panic: %v", err)
	}
	if status2 != http.StatusOK {
		t.Errorf("liveness status = %d after checker panic, want 200", status2)
	}
}

// panicChecker 是必 panic 的检查器。
type panicChecker struct{}

func (c *panicChecker) CheckHealth() error { panic("checker exploded") }

// TestRunHealthChecksConcurrent 验证 SC-03 的并发语义：两个各睡 150ms 的
// checker 在 200ms 上限内通过——顺序执行（300ms）必然超时。
func TestRunHealthChecksConcurrent(t *testing.T) {
	slow := func() lynx.Checker {
		return funcChecker(func() error {
			time.Sleep(150 * time.Millisecond)
			return nil
		})
	}
	checkers := func() []lynx.Checker { return []lynx.Checker{slow(), slow()} }

	start := time.Now()
	if err := runHealthChecks(checkers, 200*time.Millisecond); err != nil {
		t.Fatalf("并发执行下应在时限内全部通过: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Errorf("elapsed = %v, checker 未并发执行（顺序 300ms 应已超时）", elapsed)
	}
}

// TestRunHealthChecksTimeout 单元验证超时语义。
func TestRunHealthChecksTimeout(t *testing.T) {
	hung := funcChecker(func() error {
		time.Sleep(2 * time.Second)
		return nil
	})
	err := runHealthChecks(func() []lynx.Checker { return []lynx.Checker{hung} },
		50*time.Millisecond)
	if err == nil {
		t.Fatal("hung checker 应按超时不健康返回")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want 超时错误", err)
	}
}

// funcChecker 把函数适配为 lynx.Checker。
type funcChecker func() error

func (f funcChecker) CheckHealth() error { return f() }

// TestHealthCheckPrefix 回归 SC-08：WithHealthCheckPrefix 改挂载路径，
// 缺省 /healthz 端点回落到业务 handler。
func TestHealthCheckPrefix(t *testing.T) {
	addr, stop := startHTTPServerForTest(t, WithHealthCheckPrefix("/health"))
	defer stop()

	status, _, err := healthzGet(t, "http://"+addr+"/health/liveness")
	if err != nil {
		t.Fatalf("GET /health/liveness: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("/health/liveness status = %d, want 200", status)
	}

	// 缺省前缀端点不再存在：应命中业务兜底 handler（响应 "user"）。
	_, body, err := healthzGet(t, "http://"+addr+"/healthz/liveness")
	if err != nil {
		t.Fatalf("GET /healthz/liveness: %v", err)
	}
	if body != "user" {
		t.Errorf("/healthz/liveness body = %q, want 命中业务 handler（\"user\"）", body)
	}
}

// TestDisableHealthCheck 回归 SC-08：WithDisableHealthCheck 后内置端点
// 消失，请求落到业务 handler。
func TestDisableHealthCheck(t *testing.T) {
	addr, stop := startHTTPServerForTest(t, WithDisableHealthCheck())
	defer stop()

	_, body, err := healthzGet(t, "http://"+addr+"/healthz/readiness")
	if err != nil {
		t.Fatalf("GET /healthz/readiness: %v", err)
	}
	if body != "user" {
		t.Errorf("body = %q, want 业务 handler 响应（\"user\"）", body)
	}
}

// --- SC-21：HTTP TLS 路径 ---

// testTLSConfig 生成仅用于测试的自签证书（ecdsa + x509，IP 127.0.0.1），
// 参照 server/grpc 侧测试的证书生成方式，不提交证书文件。
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey() error = %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair() error = %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

// TestHTTPServerTLS 回归 SC-21：WithTLSConfig → ServeTLS 路径可用，
// TLS 客户端成功、明文客户端失败。
func TestHTTPServerTLS(t *testing.T) {
	addr, stop := startHTTPServerForTest(t, WithTLSConfig(testTLSConfig(t)))
	defer stop()

	tlsClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := tlsClient.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("TLS GET: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "user" {
		t.Errorf("TLS path: status = %d body = %q, want 200 user", resp.StatusCode, b)
	}

	// 明文请求 TLS 端口必须失败：Go 的 http.Server 对明文请求 TLS 端口
	// 返回 400（"Client sent an HTTP request to an HTTPS server"），
	// 不会命中业务 handler。
	plain := &http.Client{Timeout: 2 * time.Second}
	resp2, err := plain.Get("http://" + addr + "/")
	if err == nil {
		b2, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()
		if resp2.StatusCode == http.StatusOK || string(b2) == "user" {
			t.Errorf("plaintext GET hit the business handler: status = %d body = %q, want rejected", resp2.StatusCode, b2)
		}
	}
}
