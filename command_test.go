package lynx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sequenceChecker fails CheckHealth for the first `failures` calls, then succeeds.
type sequenceChecker struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (c *sequenceChecker) CheckHealth() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= c.failures {
		return errors.New("not ready")
	}
	return nil
}

func (c *sequenceChecker) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// depService 把 Checker 适配为可注册服务：注册路径会在 addServices 收集
// Checker 进健康聚合，命令的依赖等待经服务快照按三级优先命中 Checker 层
// ——与生产路径一致（直接改 healthCheckers 字段绕过了服务快照）。
type depService struct {
	name    string
	checker Checker
}

func (s *depService) Name() string                    { return s.name }
func (s *depService) Init(ctx AppContext) error       { return nil }
func (s *depService) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *depService) Stop(ctx context.Context) error  { return nil }
func (s *depService) CheckHealth() error              { return s.checker.CheckHealth() }

// readyService 实现三级优先的 Ready 层（可选同时实现 Checker 以验证
// 层级偏好：Ready 优先时 healthCalled 应保持 0）。delay 后关闭 channel；
// never 置位则永不关闭。
type readyService struct {
	name         string
	never        bool
	health       Checker // 非 nil 时 CheckHealth 委托给它
	ready        chan struct{}
	closeOnce    sync.Once
	healthCalled atomic.Int32
}

func newReadyService(name string, delay time.Duration, never bool, health Checker) *readyService {
	s := &readyService{name: name, never: never, health: health, ready: make(chan struct{})}
	if !never {
		time.AfterFunc(delay, func() {
			s.closeOnce.Do(func() { close(s.ready) })
		})
	}
	return s
}

func (s *readyService) Name() string          { return s.name }
func (s *readyService) Init(AppContext) error { return nil }
func (s *readyService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (s *readyService) Stop(ctx context.Context) error { return nil }
func (s *readyService) Ready() <-chan struct{}         { return s.ready }
func (s *readyService) CheckHealth() error {
	s.healthCalled.Add(1)
	if s.health == nil {
		return nil
	}
	return s.health.CheckHealth()
}

func newAppWithCheckers(t *testing.T, checkers ...Checker) App {
	t.Helper()
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	for i, c := range checkers {
		app.Register(&depService{name: fmt.Sprintf("dep-%d", i), checker: c})
	}
	return app
}

func TestNewCommandDefaults(t *testing.T) {
	cmd := NewCommand(func(ctx context.Context) error { return nil })
	c, ok := cmd.(*command)
	if !ok {
		t.Fatalf("NewCommand() type = %T, want *command", cmd)
	}
	if c.options.MaxTries != 10 {
		t.Errorf("MaxTries = %d, want 10", c.options.MaxTries)
	}
	if c.options.InitialBackoff != 100*time.Millisecond {
		t.Errorf("InitialBackoff = %v, want 100ms", c.options.InitialBackoff)
	}
	if c.options.MaxBackoff != 30*time.Second {
		t.Errorf("MaxBackoff = %v, want 30s", c.options.MaxBackoff)
	}
	if c.Name() != "command" {
		t.Errorf("Name() = %q, want %q", c.Name(), "command")
	}
}

func TestNewCommandOptions(t *testing.T) {
	cmd := NewCommand(nil, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	c := cmd.(*command)
	if c.options.MaxTries != 3 {
		t.Errorf("MaxTries = %d, want 3", c.options.MaxTries)
	}
	if c.options.InitialBackoff != time.Millisecond {
		t.Errorf("InitialBackoff = %v, want 1ms", c.options.InitialBackoff)
	}
	if c.options.MaxBackoff != 5*time.Millisecond {
		t.Errorf("MaxBackoff = %v, want 5ms", c.options.MaxBackoff)
	}
}

func TestCommandStartHealthy(t *testing.T) {
	checker := &sequenceChecker{}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	if got := checker.Calls(); got != 1 {
		t.Errorf("health checked %d times, want 1", got)
	}
}

func TestCommandStartRetriesUntilHealthy(t *testing.T) {
	checker := &sequenceChecker{failures: 2}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(5), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	start := time.Now()
	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	if got := checker.Calls(); got != 3 {
		t.Errorf("health checked %d times, want 3 (2 failures + 1 success)", got)
	}
	// Two retries with ~1ms initial backoff: must be well under a second.
	if elapsed > 2*time.Second {
		t.Errorf("Start() took %v, backoff seems not to honor WithBackoff", elapsed)
	}
}

func TestCommandStartExhaustsRetries(t *testing.T) {
	checker := &sequenceChecker{failures: 100}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := cmd.Start(context.Background())
	if err == nil {
		t.Fatal("Start() error = nil, want retry exhaustion error")
	}
	if !strings.Contains(err.Error(), "timed out waiting for dependencies to become ready") {
		t.Errorf("Start() error = %v, want it to mention %q", err, "timed out waiting for dependencies to become ready")
	}
	if got := checker.Calls(); got != 3 {
		t.Errorf("health checked %d times, want 3 (MaxTries)", got)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
}

func TestCommandStartContextCancelled(t *testing.T) {
	// With an always-failing checker and a cancelled context, the retry loop
	// must abort instead of exhausting MaxTries.
	checker := &sequenceChecker{failures: 100}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(10), WithBackoff(time.Second, 5*time.Second))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := cmd.Start(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Start() error = nil, want error for cancelled context")
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
	// Must abort on the cancelled context rather than retrying with 1s backoff.
	if elapsed > time.Second {
		t.Errorf("Start() took %v, want immediate abort on cancelled context", elapsed)
	}
	if got := checker.Calls(); got >= 10 {
		t.Errorf("health checked %d times, want fewer than MaxTries on cancelled context", got)
	}
}

func TestCommandStartHealthyWithCancelledContext(t *testing.T) {
	// A healthy checker succeeds on the first attempt, so the cancelled
	// context is never consulted by the retry loop and the command runs.
	checker := &sequenceChecker{}
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := cmd.Start(ctx); err != nil {
		t.Errorf("Start() error = %v, want nil for healthy checker", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
}

func TestCommandFnErrorPropagates(t *testing.T) {
	app := newAppWithCheckers(t)

	wantErr := errors.New("command failed")
	cmd := NewCommand(func(ctx context.Context) error {
		return wantErr
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := cmd.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want %v", err, wantErr)
	}
}

func TestCommandStopClosesApp(t *testing.T) {
	app := newAppWithCheckers(t)
	cmd := NewCommand(func(ctx context.Context) error { return nil })
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	_ = cmd.Stop(context.Background())
	select {
	case <-app.Context().Done():
	case <-time.After(time.Second):
		t.Error("Stop() should close the app")
	}
}

// hungChecker 的 CheckHealth 永久阻塞直到测试释放：暴露 CORE-11 修复前
// "单次尝试永久卡死、backoff 上限失效"的缺陷。
type hungChecker struct {
	release chan struct{}
	calls   atomic.Int32
}

func (c *hungChecker) CheckHealth() error {
	c.calls.Add(1)
	<-c.release
	return errors.New("hung checker released")
}

// TestCommandStartHungCheckerTimesOut 锁定 CORE-11：挂死的 checker 被单次
// 检查超时上界兜住，超时视为未就绪参与重试，重试耗尽后 Start 返回错误，
// 而非永久阻塞。
func TestCommandStartHungCheckerTimesOut(t *testing.T) {
	checker := &hungChecker{release: make(chan struct{})}
	defer close(checker.release)
	app := newAppWithCheckers(t, checker)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(2), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	start := time.Now()
	err := cmd.Start(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Start() error = nil, want retry exhaustion error")
	}
	if !strings.Contains(err.Error(), "timed out waiting for dependencies to become ready") {
		t.Errorf("Start() error = %v, want dependency wait error", err)
	}
	if !strings.Contains(err.Error(), "health check timed out") {
		t.Errorf("Start() error = %v, want per-check timeout to be the not-ready cause", err)
	}
	// 2 次尝试 × 3s 单次上界 + 毫秒级 backoff，总时长应落在 (5s, 10s)。
	if elapsed < 5*time.Second {
		t.Errorf("Start() took %v, want per-check timeout to actually engage", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Start() took %v, want bounded by per-check timeout × MaxTries", elapsed)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Errorf("health checked %d times, want 2 (MaxTries)", got)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
}

func TestNewCommandProbeTimeout(t *testing.T) {
	cmd := NewCommand(nil, WithProbeTimeout(250*time.Millisecond))
	c := cmd.(*command)
	if c.options.ProbeTimeout != 250*time.Millisecond {
		t.Errorf("ProbeTimeout = %v, want 250ms", c.options.ProbeTimeout)
	}
	// 非法值回落默认上界，与既有钳制风格一致。
	cmd = NewCommand(nil, WithProbeTimeout(0))
	c = cmd.(*command)
	if c.options.ProbeTimeout != defaultProbeTimeout {
		t.Errorf("ProbeTimeout = %v, want default %v", c.options.ProbeTimeout, defaultProbeTimeout)
	}
	cmd = NewCommand(nil)
	c = cmd.(*command)
	if c.options.ProbeTimeout != defaultProbeTimeout {
		t.Errorf("default ProbeTimeout = %v, want %v", c.options.ProbeTimeout, defaultProbeTimeout)
	}
}

// TestCommandStartProbeTimeoutShortened：WithProbeTimeout 收紧单次探测上界，
// 挂死 checker 场景的失败耗时由该值而非默认 3s 决定。
func TestCommandStartProbeTimeoutShortened(t *testing.T) {
	checker := &hungChecker{release: make(chan struct{})}
	defer close(checker.release)
	app := newAppWithCheckers(t, checker)

	cmd := NewCommand(func(ctx context.Context) error { return nil },
		WithMaxTries(2), WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithProbeTimeout(50*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	start := time.Now()
	err := cmd.Start(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Start() error = nil, want retry exhaustion error")
	}
	if !strings.Contains(err.Error(), "health check timed out after 50ms") {
		t.Errorf("Start() error = %v, want 50ms probe timeout as the not-ready cause", err)
	}
	// 2 次尝试 × 50ms 上界 + 毫秒级 backoff，应远小于默认上界量级。
	if elapsed > 2*time.Second {
		t.Errorf("Start() took %v, want bounded by shortened probe timeout", elapsed)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Errorf("health checked %d times, want 2 (MaxTries)", got)
	}
}

// TestCommandStartWaitsForReadyService：仅实现 Ready（无 Checker）的依赖，
// 命令等 channel 关闭后执行——Ready 层独立成立，不依赖健康检查。
func TestCommandStartWaitsForReadyService(t *testing.T) {
	app := newAppWithCheckers(t)
	rs := newReadyService("ready-dep", 100*time.Millisecond, false, nil)
	app.Register(rs)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(10), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	start := time.Now()
	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	// 下界取 delay/2：Ready 的 AfterFunc 计时在 newReadyService 时启动，
	// 早于本处计时起点——严格 >= delay 的断言在负载下会因调度偏差偶发
	// 失败（实测 99.4ms < 100ms）。不等待的实现在此下界下仍会立即暴露。
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("Start() took %v, want to wait for Ready channel", elapsed)
	}
}

// TestCommandStartReadyTierPreferred：同时实现 Ready 与 Checker 的服务走
// Ready 层——即使其 checker 永远不健康，命令仍在 channel 关闭后执行，
// 且 checker 从未被轮询。
func TestCommandStartReadyTierPreferred(t *testing.T) {
	app := newAppWithCheckers(t)
	unhealthy := &sequenceChecker{failures: 100}
	rs := newReadyService("both-dep", 50*time.Millisecond, false, unhealthy)
	app.Register(rs)

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(10), WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := cmd.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil via Ready tier", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
	if got := rs.healthCalled.Load(); got != 0 {
		t.Errorf("CheckHealth called %d times, want 0 (Ready tier must win)", got)
	}
	if got := unhealthy.Calls(); got != 0 {
		t.Errorf("underlying checker called %d times, want 0", got)
	}
}

// TestCommandStartReadyNeverClosesExhausts：永不关闭的 Ready 按单次上界
// 视为未就绪参与重试，预算耗尽后报依赖等待超时。
func TestCommandStartReadyNeverClosesExhausts(t *testing.T) {
	app := newAppWithCheckers(t)
	app.Register(newReadyService("never-dep", 0, true, nil))

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(1), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	start := time.Now()
	err := cmd.Start(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Start() error = nil, want wait timeout")
	}
	if !strings.Contains(err.Error(), "ready signal not closed") {
		t.Errorf("Start() error = %v, want per-wait bound to be the cause", err)
	}
	if !strings.Contains(err.Error(), "timed out waiting for dependencies to become ready") {
		t.Errorf("Start() error = %v, want dependency wait error", err)
	}
	if elapsed < 3*time.Second {
		t.Errorf("Start() took %v, want per-wait bound to actually engage", elapsed)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
}

// TestCommandStartReadyAbortsOnCtxCancel：依赖未就绪期间 ctx 取消（组
// 中断）→ 立即退出并报 aborted，不再等待单次上界或重试预算。
func TestCommandStartReadyAbortsOnCtxCancel(t *testing.T) {
	app := newAppWithCheckers(t)
	app.Register(newReadyService("never-dep", 0, true, nil))

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(10), WithBackoff(time.Second, 5*time.Second))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := cmd.Start(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Start() error = nil, want abort on cancelled context")
	}
	if !strings.Contains(err.Error(), "aborted waiting for dependencies") {
		t.Errorf("Start() error = %v, want aborted message", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Start() took %v, want prompt abort on ctx cancel", elapsed)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
}

// TestCommandStartReadyAlreadyClosedWithCancelledContext：已就绪的 Ready
// 在 ctx 已取消时仍立即放行（"首查即就绪即运行"语义，与 Checker 层的
// 既有取舍对齐）。channel 在 Start 之前确保已闭合，避免竞态。
func TestCommandStartReadyAlreadyClosedWithCancelledContext(t *testing.T) {
	app := newAppWithCheckers(t)
	rs := newReadyService("closed-dep", 0, false, nil)
	app.Register(rs)
	select {
	case <-rs.Ready():
	case <-time.After(time.Second):
		t.Fatal("ready channel should close after delay")
	}

	var ran atomic.Int32
	cmd := NewCommand(func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}, WithMaxTries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err := cmd.Init(app); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cmd.Start(ctx); err != nil {
		t.Errorf("Start() error = %v, want nil for already-ready dependency", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("command ran %d times, want 1", got)
	}
}

// TestCommandFallbackExternalAppContext 见包外测试
// （command_fallback_test.go，package lynx_test）：外部 AppContext 实现
// 经 lynxtest.NewContext + ContextWithCheckers 驱动，验证回退路径。
