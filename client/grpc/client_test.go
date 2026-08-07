package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// rawCodec 是仅用于测试的字节码 codec：绕过 proto 定义，直接收发
// []byte 消息。
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: cannot marshal %T", v)
	}
	return b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: cannot unmarshal into %T", v)
	}
	*p = data
	return nil
}

func (rawCodec) Name() string { return "raw" }

func TestMain(m *testing.M) {
	encoding.RegisterCodec(rawCodec{})
	os.Exit(m.Run())
}

// startEchoServer 起一个记录 incoming metadata 的最小 gRPC server
// （UnknownServiceHandler），返回监听地址与 metadata 读取函数。
func startEchoServer(t *testing.T, opts ...grpc.ServerOption) (string, func() metadata.MD) {
	t.Helper()
	var (
		mu    sync.Mutex
		gotMD metadata.MD
	)
	opts = append(opts, grpc.UnknownServiceHandler(func(srv any, stream grpc.ServerStream) error {
		mu.Lock()
		gotMD, _ = metadata.FromIncomingContext(stream.Context())
		mu.Unlock()
		return stream.SendMsg([]byte("reply"))
	}))
	srv := grpc.NewServer(opts...)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), func() metadata.MD {
		mu.Lock()
		defer mu.Unlock()
		return gotMD.Copy()
	}
}

// TestMetadataPropagation 断言 ctx 日志属性写入 outgoing metadata
// （unary 与 stream 两条路径），key 与日志字段同名。
func TestMetadataPropagation(t *testing.T) {
	addr, gotMD := startEchoServer(t)
	conn, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "rid-1"),
		slog.String(logging.FieldUserID, "u1"))

	// unary 路径。
	var reply []byte
	if err := conn.Invoke(ctx, "/test.Echo/Echo", []byte("req"), &reply,
		grpc.ForceCodec(rawCodec{})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	md := gotMD()
	if got := md.Get(logging.FieldRequestID); len(got) != 1 || got[0] != "rid-1" {
		t.Errorf("metadata request_id = %v, want [rid-1]", got)
	}
	if got := md.Get(logging.FieldUserID); len(got) != 1 || got[0] != "u1" {
		t.Errorf("metadata user_id = %v, want [u1]", got)
	}

	// stream 路径。
	sc, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true},
		"/test.Echo/Stream", grpc.ForceCodec(rawCodec{}))
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := sc.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	reply = nil
	if err := sc.RecvMsg(&reply); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	md = gotMD()
	if got := md.Get(logging.FieldRequestID); len(got) != 1 || got[0] != "rid-1" {
		t.Errorf("stream metadata request_id = %v, want [rid-1]", got)
	}
	if got := md.Get(logging.FieldUserID); len(got) != 1 || got[0] != "u1" {
		t.Errorf("stream metadata user_id = %v, want [u1]", got)
	}
}

// TestMetadataExistingNotOverwritten 断言调用方已显式设置的 metadata
// key 不被 ctx 属性覆盖。
func TestMetadataExistingNotOverwritten(t *testing.T) {
	addr, gotMD := startEchoServer(t)
	conn, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	ctx := metadata.NewOutgoingContext(
		logging.WithAttrs(context.Background(),
			slog.String(logging.FieldRequestID, "auto-rid")),
		metadata.Pairs(logging.FieldRequestID, "explicit"))

	var reply []byte
	if err := conn.Invoke(ctx, "/test.Echo/Echo", []byte("req"), &reply,
		grpc.ForceCodec(rawCodec{})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	md := gotMD()
	if got := md.Get(logging.FieldRequestID); len(got) != 1 || got[0] != "explicit" {
		t.Errorf("metadata request_id = %v, want [explicit]（已存在 key 不被覆盖）", got)
	}
}

// TestDialLazy 断言 Dial 惰性连接：对端不存在时 Dial 仍成功，
// 首次 RPC 才失败。
func TestDialLazy(t *testing.T) {
	conn, err := Dial("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial 应成功（惰性连接，不发起连接）：%v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var reply []byte
	err = conn.Invoke(ctx, "/test.Echo/Echo", []byte("req"), &reply,
		grpc.ForceCodec(rawCodec{}))
	if err == nil {
		t.Fatal("Invoke 应失败（无对端）")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("status code = %v, want %v", code, codes.Unavailable)
	}
}

// TestTimeout 断言 WithTimeout 的 per-RPC 超时注入：慢 handler 触发
// DeadlineExceeded。
func TestTimeout(t *testing.T) {
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(srv any, stream grpc.ServerStream) error {
		time.Sleep(500 * time.Millisecond)
		return stream.SendMsg([]byte("reply"))
	}))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := Dial(lis.Addr().String(), WithTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var reply []byte
	err = conn.Invoke(context.Background(), "/test.Echo/Echo", []byte("req"), &reply,
		grpc.ForceCodec(rawCodec{}))
	if code := status.Code(err); code != codes.DeadlineExceeded {
		t.Errorf("status code = %v, want %v", code, codes.DeadlineExceeded)
	}
}

// testTLSConfig 生成仅用于测试的自签证书（ecdsa + x509，IP 127.0.0.1），
// 参考 crypto/x509 CreateCertificate 标准库示例，不提交证书文件。
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lynx-test"},
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

// TestTLS 断言 WithTLSConfig 启用 TLS：客户端 TLS 握手成功并完成 RPC；
// 明文客户端访问 TLS 服务端失败。
func TestTLS(t *testing.T) {
	addr, _ := startEchoServer(t, grpc.Creds(credentials.NewTLS(testTLSConfig(t))))

	// TLS 客户端成功。
	tlsConn, err := Dial(addr, WithTLSConfig(&tls.Config{InsecureSkipVerify: true}))
	if err != nil {
		t.Fatalf("Dial(TLS): %v", err)
	}
	defer tlsConn.Close()
	var reply []byte
	if err := tlsConn.Invoke(context.Background(), "/test.Echo/Echo", []byte("req"), &reply,
		grpc.ForceCodec(rawCodec{})); err != nil {
		t.Fatalf("TLS Invoke: %v", err)
	}

	// 明文客户端失败（TLS 握手被拒）。
	plainConn, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial(plaintext): %v", err)
	}
	defer plainConn.Close()
	err = plainConn.Invoke(context.Background(), "/test.Echo/Echo", []byte("req"), &reply,
		grpc.ForceCodec(rawCodec{}))
	if err == nil {
		t.Fatal("明文客户端访问 TLS 服务端应失败")
	}
}
