package lynxtest

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// clientOpts 是拨号辅助的可调项。
type clientOpts struct {
	readyTimeout time.Duration
	grpcDial     []grpc.DialOption
}

// ClientOption 配置拨号辅助（HTTPClient/GRPCConn）的行为。
type ClientOption func(*clientOpts)

// WithReadyTimeout 设置拨号前等待 server 就绪的预算（缺省 5s）；
// CI 慢机上服务启动偏慢时放宽。
func WithReadyTimeout(d time.Duration) ClientOption {
	return func(o *clientOpts) {
		if d > 0 {
			o.readyTimeout = d
		}
	}
}

// WithGRPCDialOptions 追加 grpc.DialOption（置于套件默认的 insecure
// 凭据与直连拨号器之后，可覆盖凭据，如 TLS）。
func WithGRPCDialOptions(dial ...grpc.DialOption) ClientOption {
	return func(o *clientOpts) {
		o.grpcDial = append(o.grpcDial, dial...)
	}
}

func applyClientOpts(opts []ClientOption) clientOpts {
	o := clientOpts{readyTimeout: readyTimeout}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WaitReady 等待所有 server 的 Ready channel 关闭（总预算为整个 timeout，
// 非每个 server 独立计时）；任一超时即 t.Fatal。server 从 setup 闭包捕获。
// 由 lynxtest.Run 启动的应用优先用返回的 App.WaitReady——应用启动失败时
// 它会立即以 Run 的实际错误失败用例，而不是干等超时。
func WaitReady(t testing.TB, timeout time.Duration, servers ...lynx.Server) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, s := range servers {
		select {
		case <-s.Ready():
		case <-deadline.C:
			t.Fatalf("lynxtest: server %q not ready within %s", s.Name(), timeout)
		}
	}
}

// directTransport 返回绕过代理环境变量的 HTTP Transport（直连回环地址，
// 避免宿主机 HTTP(S)_PROXY 拦截 localhost 请求返回 502），并注册空闲
// 连接回收。DefaultTransport 被替换为非 *http.Transport 时回退自建。
func directTransport(t testing.TB) *http.Transport {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = nil
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// HTTPClient 返回访问 HTTP server 的客户端：先等待就绪（预算可用
// WithReadyTimeout 调整），再返回带默认 10s 超时的客户端；URL 用
// "http://" + s.Addr() 拼接。
func HTTPClient(t testing.TB, s *lynxhttp.Server, opts ...ClientOption) *http.Client {
	t.Helper()
	o := applyClientOpts(opts)
	WaitReady(t, o.readyTimeout, s)
	return &http.Client{Timeout: 10 * time.Second, Transport: directTransport(t)}
}

// GRPCConn 返回连接到 gRPC server 的 ClientConn：先等待就绪，默认
// insecure 凭据 + 直连拨号器（回环测试场景，绕过代理环境变量）；可用
// WithGRPCDialOptions 覆盖。t.Cleanup 自动关闭连接。
func GRPCConn(t testing.TB, s *lynxgrpc.Server, opts ...ClientOption) *grpc.ClientConn {
	t.Helper()
	o := applyClientOpts(opts)
	WaitReady(t, o.readyTimeout, s)
	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}),
	}, o.grpcDial...)
	conn, err := grpc.NewClient(s.Addr(), dialOpts...)
	if err != nil {
		t.Fatalf("lynxtest: grpc.NewClient(%q) error = %v", s.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// BufconnHTTPClient 返回经 bufconn 监听器访问 HTTP 服务的客户端（配合
// lynxhttp.WithListener 注入使用）：请求 URL 的 host 任意，如
// "http://bufconn/path"。用于免 TCP 端口的服务级测试。
func BufconnHTTPClient(t testing.TB, ln *bufconn.Listener) *http.Client {
	t.Helper()
	tr := directTransport(t)
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ln.DialContext(ctx)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}
}

// BufconnGRPCConn 返回连接到 bufconn 监听器的 ClientConn（配合
// lynxgrpc.WithListener 注入使用），默认 insecure，t.Cleanup 自动关闭。
func BufconnGRPCConn(t testing.TB, ln *bufconn.Listener, copts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	dialOpts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, copts...)
	conn, err := grpc.NewClient("passthrough:///bufconn", dialOpts...)
	if err != nil {
		t.Fatalf("lynxtest: grpc.NewClient(bufconn) error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
