package lynx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunAfterClose_ReturnsErrAppClosed 回归 D2：Close 先于 Run 执行时，
// Run 不得再跑僵尸生命周期（总线已停、post-stop 已清而 Run 照常执行），
// 直接返回 ErrAppClosed；Close 幂等。
func TestRunAfterClose_ReturnsErrAppClosed(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	app, err := NewApp(WithName("closed-before-run"))
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	app.Close()
	// Close 幂等。
	app.Close()

	if err := app.Run(); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Run() = %v, want ErrAppClosed", err)
	}
}

// miniServerService 是监听型服务的最小载体（复现 D1）：Start 监听并 Serve
// （阻塞至 listener 关闭），Stop 关闭 listener。完整镜像真实 server 的两层
// 守卫——execute 侧中断检查（R-B，框架提供）与 stopRequested 标志（R-C，
// 服务自身实现）；等价语义的真实 server 用例见 server/http 与 server/grpc
// 的 stopstart 测试。根包测试不能 import server/*（导入环）。
type miniServerService struct {
	name string
	// stopRequested 镜像 server/http 的同名守卫：Stop 先置位再读 listener；
	// Start 在 Serve 前检查——已中断后不再进入 Serve。
	stopRequested atomic.Bool
	mu            sync.Mutex
	ln            net.Listener
}

func (m *miniServerService) Name() string              { return m.name }
func (m *miniServerService) Init(ctx AppContext) error { return nil }

func (m *miniServerService) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ln = ln
	m.mu.Unlock()
	if m.stopRequested.Load() {
		_ = ln.Close()
		return nil
	}
	if err := http.Serve(ln, http.NewServeMux()); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (m *miniServerService) Stop(ctx context.Context) error {
	m.stopRequested.Store(true)
	m.mu.Lock()
	ln := m.ln
	m.ln = nil
	m.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	return nil
}

// TestRun_FastFailSiblingDoesNotLeakServerStart 回归 D1：首个服务 Start
// 快速失败触发中断时，兄弟 server 的 interrupt（Stop）可能先于其 execute
// （Start）执行——Start 必须被跳过（execute 侧守卫），或 Start 已运行时由
// Stop 关闭 listener 收尾；Run 在有界时间内返回首个错误。修复前该交错会
// 让监听服务永久 Serve、Run 挂死（独立评审实测复现率约 2/5）。循环多次
// 提高回归捕获率。
func TestRun_FastFailSiblingDoesNotLeakServerStart(t *testing.T) {
	boom := errors.New("boom")
	for i := 0; i < 10; i++ {
		func() {
			restore := saveGlobals()
			defer restore()

			app, err := newLynx(NewOptions(
				WithStopTimeout(time.Second),
				WithShutdownTimeout(time.Second),
				WithBusReadyTimeout(time.Second),
			))
			if err != nil {
				t.Fatalf("newLynx() error = %v", err)
			}
			app.Register(&failStartService{name: "failing", err: boom})
			app.Register(&miniServerService{name: "mini-http"})

			runErr := make(chan error, 1)
			go func() { runErr <- app.Run() }()
			select {
			case err := <-runErr:
				if !errors.Is(err, boom) {
					t.Errorf("iteration %d: Run() = %v, want wrapped %v", i, err, boom)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("iteration %d: Run() did not return within 5s (D1: sibling server start leaked)", i)
			}
		}()
	}
}
