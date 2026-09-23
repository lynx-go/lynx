package serverkit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestShutdownSuccess：graceful 正常返回即成功，不调用 force。
func TestShutdownSuccess(t *testing.T) {
	var forced atomic.Bool
	err := Shutdown(context.Background(), time.Second, discardLogger(), "test",
		func(context.Context) error { return nil },
		func() error { forced.Store(true); return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if forced.Load() {
		t.Fatal("force called on success")
	}
}

// TestShutdownConfigCapWins：无调用方 deadline 时配置上限生效。
func TestShutdownConfigCapWins(t *testing.T) {
	var forced atomic.Bool
	start := time.Now()
	err := Shutdown(context.Background(), 50*time.Millisecond, discardLogger(), "test",
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func() error { forced.Store(true); return nil })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %v, want bounded by config cap", elapsed)
	}
	if !forced.Load() {
		t.Fatal("force not called on timeout")
	}
}

// TestShutdownCallerDeadlineWins：调用方 deadline 比配置更短时取调用方。
func TestShutdownCallerDeadlineWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Shutdown(ctx, time.Hour, discardLogger(), "test",
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %v, want bounded by caller deadline", elapsed)
	}
}

// TestShutdownGracefulErrorPassthrough：非预算类错误原样返回，不调用 force。
func TestShutdownGracefulErrorPassthrough(t *testing.T) {
	want := errors.New("graceful boom")
	var forced atomic.Bool
	err := Shutdown(context.Background(), time.Second, discardLogger(), "test",
		func(context.Context) error { return want },
		func() error { forced.Store(true); return nil })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if forced.Load() {
		t.Fatal("force called for non-timeout graceful error")
	}
}

// TestShutdownGracefulTimeoutClassified：graceful 自身返回预算耗尽错误时
// 同样归类超时并强制关闭。
func TestShutdownGracefulTimeoutClassified(t *testing.T) {
	var forced atomic.Bool
	err := Shutdown(context.Background(), time.Second, discardLogger(), "test",
		func(context.Context) error { return context.DeadlineExceeded },
		func() error { forced.Store(true); return nil })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if !forced.Load() {
		t.Fatal("force not called for classified timeout")
	}
}

// TestShutdownForceUnblocksGraceful：graceful 阻塞且 ctx 到期时，先 force
// 解除阻塞再等其退出（模拟 grpc.GracefulStop + Stop 的形态）。
func TestShutdownForceUnblocksGraceful(t *testing.T) {
	release := make(chan struct{})
	force := func() error { close(release); return nil }
	graceful := func(context.Context) error { <-release; return nil }

	start := time.Now()
	err := Shutdown(context.Background(), 30*time.Millisecond, discardLogger(), "test", graceful, force)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %v, want force to unblock graceful promptly", elapsed)
	}
}
