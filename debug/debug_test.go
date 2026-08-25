package debug

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
)

// fakeAppContext 是 lynx.AppContext 的最小测试替身。
type fakeAppContext struct {
	logger *slog.Logger
}

func (f *fakeAppContext) Context() context.Context { return context.Background() }
func (f *fakeAppContext) Config() lynx.Config      { return nil }
func (f *fakeAppContext) Bus() eventbus.Bus        { return eventbus.NewMemoryBus(eventbus.Options{}) }
func (f *fakeAppContext) Logger(...any) *slog.Logger {
	return f.logger
}
func (f *fakeAppContext) HealthCheckers() []lynx.Checker { return nil }
func (f *fakeAppContext) Close()                         {}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewServiceDefaults(t *testing.T) {
	s := NewService()
	if s.Name() != "debug" {
		t.Errorf("Name() = %q, want %q", s.Name(), "debug")
	}
	if s.o.Addr != DefaultAddr {
		t.Errorf("Addr option = %q, want %q", s.o.Addr, DefaultAddr)
	}
	if s.o.Logger == nil {
		t.Error("Logger should not be nil")
	}
	if addr := s.Addr(); addr != "" {
		t.Errorf("Addr() before Start = %q, want empty", addr)
	}
}

func TestNewServiceOptions(t *testing.T) {
	logger := discardLogger()
	s := NewService(WithAddr("127.0.0.1:12345"), WithLogger(logger))
	if s.o.Addr != "127.0.0.1:12345" {
		t.Errorf("Addr option = %q, want %q", s.o.Addr, "127.0.0.1:12345")
	}
	if s.o.Logger != logger {
		t.Error("Logger was not set via WithLogger")
	}
	if !s.o.loggerSet {
		t.Error("loggerSet should be true after WithLogger")
	}
}

func TestNewServiceNilLoggerFallsBack(t *testing.T) {
	s := NewService(WithLogger(nil))
	if s.o.Logger == nil {
		t.Error("Logger should fall back to slog.Default()")
	}
}

func TestInitNil(t *testing.T) {
	s := NewService()
	if err := s.Init(nil); err != nil {
		t.Errorf("Init(nil) error = %v, want nil", err)
	}
	if s.logger == nil {
		t.Error("logger should not be nil after Init(nil)")
	}
}

func TestInitUsesCtxLogger(t *testing.T) {
	ctxLogger := discardLogger()
	s := NewService()
	if err := s.Init(&fakeAppContext{logger: ctxLogger}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if s.logger != ctxLogger {
		t.Error("Init should use ctx.Logger when WithLogger not set")
	}
}

func TestInitKeepsExplicitLogger(t *testing.T) {
	explicit := discardLogger()
	s := NewService(WithLogger(explicit))
	if err := s.Init(&fakeAppContext{logger: discardLogger()}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if s.logger != explicit {
		t.Error("Init should not override explicit WithLogger")
	}
}

// startService 在随机端口启动服务，返回 Start 的完成通道。
func startService(t *testing.T) (*Service, context.CancelFunc, <-chan error) {
	t.Helper()
	s := NewService(WithAddr("127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	return s, cancel, done
}

// waitServing 轮询等待服务开始监听并响应 /healthz，返回实际地址。
func waitServing(t *testing.T, s *Service) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		addr := s.Addr()
		if addr == "" {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return addr
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("debug service did not start serving within 5s")
	return ""
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s error = %v", url, err)
	}
	return resp.StatusCode, string(body)
}

func TestPprofEndpoints(t *testing.T) {
	s, cancel, done := startService(t)
	defer cancel()
	addr := waitServing(t, s)

	status, body := getBody(t, "http://"+addr+"/debug/pprof/")
	if status != http.StatusOK {
		t.Errorf("GET /debug/pprof/ status = %d, want 200", status)
	}
	if !strings.Contains(body, "Types of profiles available") {
		t.Error("GET /debug/pprof/ body missing index text")
	}

	for _, path := range []string{"/debug/pprof/heap", "/debug/pprof/goroutine?debug=1"} {
		status, _ := getBody(t, "http://"+addr+path)
		if status != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, status)
		}
	}

	status, body = getBody(t, "http://"+addr+"/debug/pprof/cmdline")
	if status != http.StatusOK {
		t.Errorf("GET /debug/pprof/cmdline status = %d, want 200", status)
	}
	if body == "" {
		t.Error("GET /debug/pprof/cmdline body should not be empty")
	}

	status, _ = getBody(t, "http://"+addr+"/debug/pprof/symbol")
	if status != http.StatusOK {
		t.Errorf("GET /debug/pprof/symbol status = %d, want 200", status)
	}

	status, _ = getBody(t, "http://"+addr+"/healthz")
	if status != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", status)
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}
}

func TestCheckHealth(t *testing.T) {
	s := NewService()
	if err := s.CheckHealth(); err == nil {
		t.Error("CheckHealth() before Start = nil, want error")
	}

	s, cancel, done := startService(t)
	waitServing(t, s)
	if err := s.CheckHealth(); err != nil {
		t.Errorf("CheckHealth() while running = %v, want nil", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}
	if err := s.CheckHealth(); err == nil {
		t.Error("CheckHealth() after Stop = nil, want error")
	}
}

func TestStopRefusesConnections(t *testing.T) {
	s, cancel, done := startService(t)
	defer cancel()
	addr := waitServing(t, s)

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		conn.Close()
		t.Error("connection to stopped debug server should be refused")
	}
}

func TestStopBeforeStart(t *testing.T) {
	s := NewService()
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop() before Start error = %v, want nil", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Errorf("Start() after Stop error = %v, want nil", err)
	}
	if err := s.CheckHealth(); err == nil {
		t.Error("CheckHealth() after Stop-before-Start = nil, want error")
	}
}

func TestStartStopConcurrent(t *testing.T) {
	for i := 0; i < 10; i++ {
		s, cancel, done := startService(t)
		stopDone := make(chan error, 1)
		go func() { stopDone <- s.Stop(context.Background()) }()
		if err := <-stopDone; err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil && !strings.Contains(err.Error(), "stopped before start") {
				t.Fatalf("Start() error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start() did not return after Stop + cancel")
		}
	}
}

func TestStartListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()
	s := NewService(WithAddr(ln.Addr().String()))
	if err := s.Start(context.Background()); err == nil {
		t.Error("Start() error = nil, want listen error")
	}
}

// TestStopForcesCloseOnTimeout 回归：存在活跃连接（半截请求）时优雅关停
// 超过 ctx deadline 应强制关闭连接并返回错误，不得无限挂起。
func TestStopForcesCloseOnTimeout(t *testing.T) {
	s, cancel, done := startService(t)
	defer cancel()
	addr := waitServing(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /debug/pprof/ HTTP/1.1\r\nHost: debug\r\n"))

	ctx, cancelStop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelStop()
	if err := s.Stop(ctx); err == nil {
		t.Error("Stop() error = nil, want graceful shutdown timeout error")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}
}

// TestServeErrorNoPanic 回归：Serve 以非 ErrServerClosed 错误返回时
// 只记日志，不 panic 不影响后续关停。
func TestServeErrorNoPanic(t *testing.T) {
	s, cancel, done := startService(t)
	defer cancel()
	waitServing(t, s)

	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln == nil {
		t.Fatal("listener should not be nil while running")
	}
	_ = ln.Close()

	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}
}

func TestServiceReadyAfterListen(t *testing.T) {
	s := NewService(WithAddr("127.0.0.1:0"), WithLogger(discardLogger()))
	select {
	case <-s.Ready():
		t.Fatal("Ready() closed before Start")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	select {
	case <-s.Ready():
	case err := <-done:
		t.Fatalf("Start() returned before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Ready() did not close after Listen")
	}
	if s.Addr() == "" {
		t.Error("Addr() empty after Ready closed")
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after Stop()")
	}
}

func TestServiceReadyNotClosedOnListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	s := NewService(WithAddr(ln.Addr().String()), WithLogger(discardLogger()))
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start() = nil, want listen error")
	}
	select {
	case <-s.Ready():
		t.Fatal("Ready() closed on Listen failure")
	default:
	}
}

// TestStopClearsAddr 锁定复审 I：Stop 关停路径与 Start 的 ctx.Done 退出
// 路径必须一致地清理 listener——Stop 之后 Addr() 返回空字符串，而非
// 残留已关闭监听器的地址。
func TestStopClearsAddr(t *testing.T) {
	s, cancel, done := startService(t)
	defer cancel()
	waitServing(t, s)
	if s.Addr() == "" {
		t.Fatal("Addr() should be non-empty while serving")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if addr := s.Addr(); addr != "" {
		t.Fatalf("Addr() after Stop = %q, want empty", addr)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}
}

// TestStartCtxCancelReleasesPort 回归 AUX-05：独立使用（Init(nil)/直接
// Start、未经 Stop）时，ctx 取消退出路径必须关闭 httpServer 释放端口，
// 否则 listener 持续占用到进程退出。
func TestStartCtxCancelReleasesPort(t *testing.T) {
	s := NewService(WithAddr("127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	// 等待监听就绪并记录实际端口。
	deadline := time.Now().Add(5 * time.Second)
	var addr string
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != "" {
			addr = a
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("debug service did not start listening within 5s")
	}

	// 不调用 Stop，直接取消 Start 的 ctx（独立使用路径）。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	// 端口必须已释放：同地址可立即重新 Listen。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s not released after ctx cancel: %v", addr, err)
	}
	_ = ln.Close()
}
