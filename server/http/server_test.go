package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"gocloud.dev/server/health"
	"gocloud.dev/server/requestlog"
)

func TestNewServerDefaults(t *testing.T) {
	s := NewServer(NewRouter())
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

	s := NewServer(NewRouter(),
		WithAddr("127.0.0.1:18080"),
		WithTimeout(5*time.Second),
		WithRequestLog(true),
		WithLogger(logger),
		WithHealthCheck(func() []health.Checker { return nil }),
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
	if s.o.HealthCheck == nil {
		t.Error("HealthCheck was not set via WithHealthCheck")
	}
}

func TestServerName(t *testing.T) {
	s := NewServer(NewRouter())
	if got := s.Name(); got != "http" {
		t.Errorf("Name() = %q, want %q", got, "http")
	}
}

func TestServerInit(t *testing.T) {
	s := NewServer(NewRouter())
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
	l.Log(&requestlog.Entry{
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
	handler := NewRouter()
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

	srv.Stop(context.Background())
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
	defer srv.Stop(context.Background())

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	srv.Stop(context.Background())
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
	defer srv.Stop(context.Background())

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
	defer srv.Stop(context.Background())

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

	srv.Stop(context.Background())
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

var _ lynx.Component = (*Server)(nil)

// TestStopBeforeStartIsNoop ensures Stop is safe before Start has assigned
// the underlying server (e.g. shutdown during startup), and does not panic
// on the nil embedded server.
func TestStopBeforeStartIsNoop(t *testing.T) {
	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Stop(context.Background())
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
	srv.Stop(context.Background())
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
