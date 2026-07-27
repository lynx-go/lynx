package interceptor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoggingPassthrough(t *testing.T) {
	interceptor := Logging(testLogger())
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "resp", nil
	})
	if err != nil {
		t.Fatalf("interceptor error = %v, want nil", err)
	}
	if resp != "resp" {
		t.Errorf("resp = %v, want %q", resp, "resp")
	}
}

func TestLoggingHandlerError(t *testing.T) {
	interceptor := Logging(testLogger())
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	wantErr := errors.New("handler failed")
	_, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("interceptor error = %v, want %v", err, wantErr)
	}
}

func TestRecoveryPassthrough(t *testing.T) {
	interceptor := Recovery()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "resp", nil
	})
	if err != nil {
		t.Fatalf("interceptor error = %v, want nil", err)
	}
	if resp != "resp" {
		t.Errorf("resp = %v, want %q", resp, "resp")
	}
}

func TestRecoveryRecoversPanic(t *testing.T) {
	interceptor := Recovery()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		panic("boom")
	})
	if err == nil {
		t.Fatal("interceptor error = nil, want recovered panic error")
	}
	if resp != nil {
		t.Errorf("resp = %v, want nil", resp)
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("status code = %v, want %v", code, codes.Internal)
	}
}
