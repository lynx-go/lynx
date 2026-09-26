package eventbus

import (
	"context"
	"log/slog"
	"testing"

	"github.com/lynx-go/lynx/logging"
)

// TestBuildRawEventValidatesPropagationValues：发布侧对 request_id/user_id
// 做共享校验（internal/propagation）——非法 ctx 值不进入消息头，合法值
// 照常传播；自定义白名单键不受校验。
func TestBuildRawEventValidatesPropagationValues(t *testing.T) {
	bus := NewMemoryBus(Options{})
	keys := []string{logging.FieldRequestID, logging.FieldUserID, "custom_key"}

	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldRequestID, "bad id"), // 非法：不传播
		slog.String(logging.FieldUserID, "user-ok"),   // 合法
		slog.String("custom_key", "any value works"),  // 自定义键：不校验
	)
	ev, err := BuildRawEvent(ctx, bus, "t", map[string]string{"k": "v"}, &PublishOptions{}, keys)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if _, ok := ev.Headers[logging.FieldRequestID]; ok {
		t.Errorf("invalid request_id leaked into headers: %q", ev.Headers[logging.FieldRequestID])
	}
	if got := ev.Headers[logging.FieldUserID]; got != "user-ok" {
		t.Errorf("user_id = %q, want user-ok", got)
	}
	if got := ev.Headers["custom_key"]; got != "any value works" {
		t.Errorf("custom key = %q, want passthrough without validation", got)
	}

	ctx = logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "rid-ok"))
	ev, err = BuildRawEvent(ctx, bus, "t", map[string]string{"k": "v"}, &PublishOptions{}, keys)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if got := ev.Headers[logging.FieldRequestID]; got != "rid-ok" {
		t.Errorf("valid request_id = %q, want rid-ok", got)
	}
}

// TestInvokeHandlerValidatesPropagationHeaders：消费侧对 request_id/user_id
// 做共享校验——非法头值不还原进 handler 日志属性，合法值照常还原。
func TestInvokeHandlerValidatesPropagationHeaders(t *testing.T) {
	resolver := NewResolver(Options{})
	ev := &RawEvent{
		ID:    "m1",
		Topic: "t",
		Headers: map[string]string{
			logging.FieldRequestID: "bad id",
			logging.FieldUserID:    "user-ok",
		},
	}
	var attrs []slog.Attr
	err := InvokeHandler(context.Background(), slog.Default(),
		func(ctx context.Context, _ *RawEvent) error {
			attrs = logging.AttrsFrom(ctx)
			return nil
		}, ev, resolver, InvokeOptions{Topic: "t", HandlerName: "h"})
	if err != nil {
		t.Fatalf("InvokeHandler: %v", err)
	}
	for _, a := range attrs {
		if a.Key == logging.FieldRequestID {
			t.Errorf("invalid request_id restored into handler attrs: %q", a.Value.String())
		}
	}
	found := false
	for _, a := range attrs {
		if a.Key == logging.FieldUserID && a.Value.String() == "user-ok" {
			found = true
		}
	}
	if !found {
		t.Errorf("valid user_id not restored: %v", attrs)
	}
}
