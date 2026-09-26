package grpc

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	clientgrpc "github.com/lynx-go/lynx/client/grpc"
	"github.com/lynx-go/lynx/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// idCaptureService 记录 handler ctx 中的 request_id（raw codec，无 protobuf
// 依赖；请求体不参与断言）。
type idCaptureService struct {
	mu  sync.Mutex
	rid string
}

func (s *idCaptureService) handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	if err := dec(&struct{}{}); err != nil {
		return nil, err
	}
	record := func(ctx context.Context, _ any) (any, error) {
		s.mu.Lock()
		s.rid = RequestIDFrom(ctx)
		s.mu.Unlock()
		return &struct{}{}, nil
	}
	if interceptor == nil {
		return record(ctx, nil)
	}
	return interceptor(ctx, &struct{}{}, &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/test.ID/WhoAmI",
	}, record)
}

func (s *idCaptureService) requestID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rid
}

// TestPropagationClosedLoop：client/grpc（outgoing metadata）→ server/grpc
// 内置拦截器（解析 + 回写）→ handler 可见 request_id → 响应 metadata 回写
// 同一值。此前两侧各自有测试，但缺这条端到端 join。
func TestPropagationClosedLoop(t *testing.T) {
	addr := freeAddr(t)
	s := NewServer(WithAddr(addr))
	svc := &idCaptureService{}
	s.GetServer().RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.ID",
		HandlerType: (*interface{})(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "WhoAmI", Handler: svc.handler}},
		Streams:     []grpc.StreamDesc{},
		Metadata:    "test.proto",
	}, svc)

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	waitRunning(t, s)

	conn, err := clientgrpc.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "rid-loop"),
		slog.String(logging.FieldUserID, "user-loop"))
	var header metadata.MD
	if err := conn.Invoke(ctx, "/test.ID/WhoAmI", &struct{}{}, &struct{}{},
		grpc.ForceCodec(rawCodec{}), grpc.Header(&header)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := svc.requestID(); got != "rid-loop" {
		t.Fatalf("server handler request_id = %q, want rid-loop", got)
	}
	if got := header.Get(RequestIDHeader); len(got) != 1 || got[0] != "rid-loop" {
		t.Fatalf("response metadata x-request-id = %v, want [rid-loop]", got)
	}

	_ = s.Stop(context.Background())
	select {
	case <-startErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestPropagationDisabled：WithDisableRequestID 关闭解析/生成/回写——handler
// 看不到 request_id，响应 metadata 无回写。
func TestPropagationDisabled(t *testing.T) {
	addr := freeAddr(t)
	s := NewServer(WithAddr(addr), WithDisableRequestID())
	svc := &idCaptureService{}
	s.GetServer().RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.ID",
		HandlerType: (*interface{})(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "WhoAmI", Handler: svc.handler}},
		Streams:     []grpc.StreamDesc{},
		Metadata:    "test.proto",
	}, svc)

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	waitRunning(t, s)

	conn, err := clientgrpc.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "rid-off"))
	var header metadata.MD
	if err := conn.Invoke(ctx, "/test.ID/WhoAmI", &struct{}{}, &struct{}{},
		grpc.ForceCodec(rawCodec{}), grpc.Header(&header)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := svc.requestID(); got != "" {
		t.Fatalf("handler request_id = %q, want empty when disabled", got)
	}
	if got := header.Get(RequestIDHeader); len(got) != 0 {
		t.Fatalf("response metadata x-request-id = %v, want none when disabled", got)
	}

	_ = s.Stop(context.Background())
	select {
	case <-startErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}
