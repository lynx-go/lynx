package eventbus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/logging"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testEvent() *RawEvent {
	return &RawEvent{Topic: "t", Headers: map[string]string{}, Payload: []byte("{}")}
}

// TestInvokeHandlerSuccess：成功路径单次调用、无错误。
func TestInvokeHandlerSuccess(t *testing.T) {
	r := NewResolver(Options{})
	var calls atomic.Int32
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		calls.Add(1)
		return nil
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 3}})
	if err != nil || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d, want nil, 1", err, calls.Load())
	}
}

// TestInvokeHandlerRetryThenSuccess：预算内重试至成功。
func TestInvokeHandlerRetryThenSuccess(t *testing.T) {
	r := NewResolver(Options{})
	var calls atomic.Int32
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		if calls.Add(1) < 3 {
			return errors.New("not yet")
		}
		return nil
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 3}})
	if err != nil || calls.Load() != 3 {
		t.Fatalf("err = %v, calls = %d, want nil, 3", err, calls.Load())
	}
}

// TestInvokeHandlerExhaustion：重试耗尽返回终态错误（适配器据此 Nack/丢弃）。
func TestInvokeHandlerExhaustion(t *testing.T) {
	r := NewResolver(Options{})
	var calls atomic.Int32
	wantErr := errors.New("always fails")
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		calls.Add(1)
		return wantErr
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 2}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (1 + 2 retries)", calls.Load())
	}
}

// TestInvokeHandlerFixedBackoff：退避是固定间隔（WK-17），总耗时覆盖每次重试。
func TestInvokeHandlerFixedBackoff(t *testing.T) {
	r := NewResolver(Options{})
	start := time.Now()
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		return errors.New("fail")
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 2, Backoff: 20 * time.Millisecond}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err = nil, want exhaustion error")
	}
	if elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 2 fixed backoffs (40ms)", elapsed)
	}
}

// TestInvokeHandlerCtxCancelDuringBackoff：退避中取消立即返回 ctx.Err()，
// 不陪跑剩余退避与重试预算。
func TestInvokeHandlerCtxCancelDuringBackoff(t *testing.T) {
	r := NewResolver(Options{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	err := InvokeHandler(ctx, discardLogger(), func(context.Context, *RawEvent) error {
		return errors.New("fail")
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 5, Backoff: time.Second}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want prompt abort during backoff", elapsed)
	}
}

// TestInvokeHandlerOnce：AutoAck 语义——只调用一次、错误吞掉。
func TestInvokeHandlerOnce(t *testing.T) {
	r := NewResolver(Options{})
	var calls atomic.Int32
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		calls.Add(1)
		return errors.New("ignored")
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 3}, Once: true})
	if err != nil || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d, want nil, 1", err, calls.Load())
	}
}

// TestInvokeHandlerSwallow：ContinueOnError 语义——失败记录后吞掉、不重试。
func TestInvokeHandlerSwallow(t *testing.T) {
	r := NewResolver(Options{})
	var calls atomic.Int32
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		calls.Add(1)
		return errors.New("ignored")
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 3}, Swallow: true})
	if err != nil || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d, want nil, 1", err, calls.Load())
	}
}

// TestInvokeHandlerTimeout：单次尝试超时按终态失败处理——挂死的 handler
// 不会永久占用调用方（在途槽位释放），返回 deadline exceeded。
func TestInvokeHandlerTimeout(t *testing.T) {
	r := NewResolver(Options{})
	release := make(chan struct{})
	defer close(release) // 释放挂起的 handler goroutine，避免测试泄漏
	start := time.Now()
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		<-release
		return nil
	}, testEvent(), r, InvokeOptions{
		Topic: "t", HandlerName: "h",
		Retry:   RetryOptions{MaxRetries: 0},
		Timeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, want prompt timeout", elapsed)
	}
}

// TestInvokeHandlerTimeoutRetries：超时可重试——每次尝试各有独立超时。
func TestInvokeHandlerTimeoutRetries(t *testing.T) {
	r := NewResolver(Options{})
	var calls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		calls.Add(1)
		<-release
		return nil
	}, testEvent(), r, InvokeOptions{
		Topic: "t", HandlerName: "h",
		Retry:   RetryOptions{MaxRetries: 1},
		Timeout: 20 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 (1 + 1 retry, each timed out)", got)
	}
}

// TestInvokeHandlerTimeoutCooperative：handler 尊重 ctx 时自行返回超时错误。
func TestInvokeHandlerTimeoutCooperative(t *testing.T) {
	r := NewResolver(Options{})
	err := InvokeHandler(context.Background(), discardLogger(), func(ctx context.Context, _ *RawEvent) error {
		<-ctx.Done()
		return ctx.Err()
	}, testEvent(), r, InvokeOptions{
		Topic: "t", HandlerName: "h",
		Retry:   RetryOptions{MaxRetries: 0},
		Timeout: 30 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestInvokeHandlerNoTimeoutByDefault：默认不设超时——慢 handler 正常返回。
func TestInvokeHandlerNoTimeoutByDefault(t *testing.T) {
	r := NewResolver(Options{})
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}, testEvent(), r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 0}})
	if err != nil {
		t.Fatalf("err = %v, want nil (no timeout by default)", err)
	}
}

// TestInvokeHandlerPropagatesHeaders：发布侧传播键还原进 handler ctx。
func TestInvokeHandlerPropagatesHeaders(t *testing.T) {
	r := NewResolver(Options{})
	ev := testEvent()
	ev.Headers["request_id"] = "req-1"
	var got string
	err := InvokeHandler(context.Background(), discardLogger(), func(ctx context.Context, _ *RawEvent) error {
		for _, a := range logging.AttrsFrom(ctx) {
			if a.Key == logging.FieldRequestID {
				got = a.Value.String()
			}
		}
		return nil
	}, ev, r, InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 1}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "req-1" {
		t.Fatalf("request_id attr = %q, want req-1", got)
	}
}
