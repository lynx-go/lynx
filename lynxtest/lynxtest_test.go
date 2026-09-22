package lynxtest_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/lynxtest"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

func slogDefault() *slog.Logger { return slog.Default() }

func newHTTPSetup(hs **lynxhttp.Server, listening chan<- string) lynx.SetupFunc {
	return func(a lynx.App) error {
		mux := http.NewServeMux()
		mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "hello "+r.URL.Query().Get("name"))
		})
		*hs = lynxhttp.NewServer(mux,
			lynxhttp.WithAddr(a.Config().GetString("server.addr")),
			lynxhttp.WithHealthCheckers(a.HealthCheckers),
		)
		a.Register(*hs)
		// 组装测试顺带验证事件接线：server 的 listening 事件送达订阅者。
		return a.Bus().Subscribe(context.Background(), eventbus.TopicHTTPListening,
			func(_ context.Context, ev *eventbus.RawEvent) error {
				listening <- ev.Topic
				return nil
			})
	}
}

func TestRun_HTTPApp(t *testing.T) {
	var hs *lynxhttp.Server
	listening := make(chan string, 1)
	app := lynxtest.Run(t, newHTTPSetup(&hs, listening), lynxtest.WithConfigMap(map[string]any{
		"service.name": "http-test",
		"server.addr":  ":0",
	}))

	if got := app.Config().GetString("service.name"); got != "http-test" {
		t.Errorf("service.name = %q, want %q", got, "http-test")
	}
	if m := lynx.Meta(app.Context()); m.Name != "http-test" {
		t.Errorf("Meta().Name = %q, want %q", m.Name, "http-test")
	}

	client := lynxtest.HTTPClient(t, hs)

	resp, err := client.Get("http://" + hs.Addr() + "/hello?name=lynx")
	if err != nil {
		t.Fatalf("Get(/hello) error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "hello lynx" {
		t.Errorf("GET /hello = %d %q, want %d %q", resp.StatusCode, string(body), http.StatusOK, "hello lynx")
	}

	// 内置健康端点与 app 级检查器聚合是组装测试的核心断言之一。
	hresp, err := client.Get("http://" + hs.Addr() + "/healthz/liveness")
	if err != nil {
		t.Fatalf("Get(/healthz/liveness) error = %v", err)
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz/liveness = %d, want %d", hresp.StatusCode, http.StatusOK)
	}

	select {
	case topic := <-listening:
		if topic != eventbus.TopicHTTPListening {
			t.Errorf("listening event topic = %q, want %q", topic, eventbus.TopicHTTPListening)
		}
	case <-time.After(3 * time.Second):
		t.Error("did not receive TopicHTTPListening event within 3s")
	}
}

func TestRun_ConfigYAML(t *testing.T) {
	app := lynxtest.Run(t, func(a lynx.App) error { return nil },
		lynxtest.WithConfigYAML(`
service:
  name: yaml-app
  version: 0.1.0
`))
	if got := app.Config().GetString("service.name"); got != "yaml-app" {
		t.Errorf("service.name = %q, want %q", got, "yaml-app")
	}
	if got := app.Config().GetString("service.version"); got != "0.1.0" {
		t.Errorf("service.version = %q, want %q", got, "0.1.0")
	}
}

// TestRun_ConfigLayering 验证配置叠加顺序：文件基线 → YAML → Map（最高）。
func TestRun_ConfigLayering(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("service:\n  name: file-app\nhttp:\n  addr: ':9999'\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	app := lynxtest.Run(t, func(a lynx.App) error { return nil },
		lynxtest.WithConfigBaseline(file),
		lynxtest.WithConfigMap(map[string]any{"service.name": "map-app"}))

	// Map 覆盖文件值，未覆盖的键沿用文件基线。
	if got := app.Config().GetString("service.name"); got != "map-app" {
		t.Errorf("service.name = %q, want %q (map should win)", got, "map-app")
	}
	if got := app.Config().GetString("http.addr"); got != ":9999" {
		t.Errorf("http.addr = %q, want %q (file base)", got, ":9999")
	}
}

// TestRun_DefaultConfigHermetic 验证裸 Run 的封闭性：包目录里散落的
// config.yaml 不会被隐式加载（Run 总是注入内存配置，不走 os.Args/文件装配）。
func TestRun_DefaultConfigHermetic(t *testing.T) {
	const stray = "config.yaml"
	if _, err := os.Stat(stray); err == nil {
		t.Skip("config.yaml already exists in package dir")
	}
	if err := os.WriteFile(stray, []byte("service:\n  name: leaked\n"), 0o600); err != nil {
		t.Skipf("cannot create stray config: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(stray) })

	app := lynxtest.Run(t, func(a lynx.App) error { return nil })
	if got := app.Config().GetString("service.name"); got != "" {
		t.Errorf("service.name = %q, want empty (stray config.yaml leaked into bare Run)", got)
	}
}

func TestRun_GRPCApp(t *testing.T) {
	var gs *lynxgrpc.Server
	lynxtest.Run(t, func(a lynx.App) error {
		gs = lynxgrpc.NewServer(
			lynxgrpc.WithAddr(a.Config().GetString("server.addr")),
			lynxgrpc.WithRequestLog(false),
		)
		a.Register(gs)
		return nil
	}, lynxtest.WithConfigMap(map[string]any{
		"service.name": "grpc-test",
		"server.addr":  ":0",
	}))

	conn := lynxtest.GRPCConn(t, gs)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check() error = %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("health status = %v, want SERVING", resp.Status)
	}
}

func TestRun_BufconnServers(t *testing.T) {
	hln := bufconn.Listen(64 * 1024)
	gln := bufconn.Listen(64 * 1024)

	var hs *lynxhttp.Server
	var gs *lynxgrpc.Server
	app := lynxtest.Run(t, func(a lynx.App) error {
		mux := http.NewServeMux()
		mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "hi")
		})
		hs = lynxhttp.NewServer(mux, lynxhttp.WithListener(hln))
		gs = lynxgrpc.NewServer(lynxgrpc.WithListener(gln), lynxgrpc.WithRequestLog(false))
		a.Register(hs, gs)
		return nil
	}, lynxtest.WithConfigMap(map[string]any{"service.name": "bufconn-test"}))

	// 句柄版 WaitReady：应用若先于就绪退出会立即带出 Run 的实际错误。
	app.WaitReady(t, 5*time.Second, hs, gs)

	hc := lynxtest.BufconnHTTPClient(t, hln)
	resp, err := hc.Get("http://bufconn/hello")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "hi" {
		t.Errorf("GET /hello = %d %q, want %d %q", resp.StatusCode, string(body), http.StatusOK, "hi")
	}

	conn := lynxtest.BufconnGRPCConn(t, gln)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gresp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check() error = %v", err)
	}
	if gresp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("health status = %v, want SERVING", gresp.Status)
	}
}

// TestRun_RestoresGlobals 验证 Run 的全局恢复契约：内层用例（含 cleanup）
// 结束后，三个进程级全局回到 Run 前的值。
func TestRun_RestoresGlobals(t *testing.T) {
	prevLynx := lynx.Get()
	prevBus := eventbus.Default()
	prevSlog := slogDefault()

	t.Run("app", func(t *testing.T) {
		var hs *lynxhttp.Server
		listening := make(chan string, 1)
		lynxtest.Run(t, newHTTPSetup(&hs, listening), lynxtest.WithConfigMap(map[string]any{
			"service.name": "globals-test",
			"server.addr":  ":0",
		}))
		lynxtest.HTTPClient(t, hs)
	})

	if lynx.Get() != prevLynx {
		t.Errorf("lynx.Get() changed after test cleanup (want restored)")
	}
	if eventbus.Default() != prevBus {
		t.Errorf("eventbus.Default() changed after test cleanup (want restored)")
	}
	if slogDefault() != prevSlog {
		t.Errorf("slog.Default() changed after test cleanup (want restored)")
	}
}

// greeterService 是 L1 单测的示例被测服务：Init 从 AppContext 取配置。
type greeterService struct {
	name string
}

func (g *greeterService) Name() string { return "greeter" }

func (g *greeterService) Init(ctx lynx.AppContext) error {
	g.name = ctx.Config().GetString("greeter.name")
	return nil
}

func (g *greeterService) Start(ctx context.Context) error { return nil }

func (g *greeterService) Stop(ctx context.Context) error { return nil }

func (g *greeterService) Greet(who string) string {
	return "hello " + who + " (" + g.name + ")"
}

func TestNewContext_ConfigYAML(t *testing.T) {
	actx := lynxtest.NewContext(t,
		lynxtest.ContextWithConfigYAML("greeter:\n  name: yaml-ctx\n"))
	if got := actx.Config().GetString("greeter.name"); got != "yaml-ctx" {
		t.Errorf("greeter.name = %q, want %q", got, "yaml-ctx")
	}
}

func TestNewContext_ServiceUnitTest(t *testing.T) {
	actx := lynxtest.NewContext(t,
		lynxtest.ContextWithConfigMap(map[string]any{"greeter.name": "test"}))

	svc := &greeterService{}
	if err := svc.Init(actx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if got := svc.Greet("lynx"); got != "hello lynx (test)" {
		t.Errorf("Greet() = %q, want %q", got, "hello lynx (test)")
	}

	// 总线可用：订阅后发布即达。
	got := make(chan *eventbus.RawEvent, 1)
	if err := actx.Bus().Subscribe(context.Background(), "demo.topic",
		func(_ context.Context, ev *eventbus.RawEvent) error {
			got <- ev
			return nil
		}); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := actx.Bus().Publish(context.Background(), "demo.topic", "ping"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case ev := <-got:
		if ev.Topic != "demo.topic" {
			t.Errorf("event topic = %q, want %q", ev.Topic, "demo.topic")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive published event within 2s")
	}

	// 日志接到测试输出（-v 可见）。
	actx.Logger("svc", "greeter").Info("hello from test context")
}
