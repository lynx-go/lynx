package propagation

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lynx-go/lynx/logging"
)

// TestValid 钉住入站/传播值校验：合法（UUID/常见追踪 ID）通过，超长与
// 非法字符被拒。
func TestValid(t *testing.T) {
	valid := []string{
		"rid-from-client",
		"0123456789abcdef",
		"aBcD-_1234",
		strings.Repeat("a", MaxValueLength),
	}
	for _, v := range valid {
		if !Valid(v) {
			t.Errorf("Valid(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", MaxValueLength+1),
		"rid with space",
		"rid<>script",
		"中文rid",
	}
	for _, v := range invalid {
		if Valid(v) {
			t.Errorf("Valid(%q) = true, want false", v)
		}
	}
}

// TestAttrs：合法值构造日志属性；非法/空值跳过对应字段。
func TestAttrs(t *testing.T) {
	attrs := Attrs("rid-1", "user-1")
	if len(attrs) != 2 ||
		attrs[0].Key != logging.FieldRequestID || attrs[0].Value.String() != "rid-1" ||
		attrs[1].Key != logging.FieldUserID || attrs[1].Value.String() != "user-1" {
		t.Fatalf("Attrs = %v, want request_id/user_id", attrs)
	}
	if got := Attrs("rid-1", "bad user"); len(got) != 1 || got[0].Key != logging.FieldRequestID {
		t.Errorf("Attrs with invalid user_id = %v, want only request_id", got)
	}
	if got := Attrs("", ""); len(got) != 0 {
		t.Errorf("Attrs(empty) = %v, want none", got)
	}
}

// TestOutbound 钉住出站传播规则：白名单键映射、request_id → user_id 顺序、
// set 返回 false（目标已存在）不覆盖、ctx 无属性时 set 不被调用。
func TestOutbound(t *testing.T) {
	ctx := logging.WithAttrs(context.Background(),
		slog.String(logging.FieldUserID, "user-1"),
		slog.String(logging.FieldRequestID, "rid-1"))

	var got []string
	added := Outbound(ctx, func(key, value string) bool {
		got = append(got, key+"="+value)
		return true
	})
	if !added {
		t.Fatal("Outbound = false, want true")
	}
	if len(got) != 2 || got[0] != RequestIDHeader+"=rid-1" || got[1] != UserIDHeader+"=user-1" {
		t.Fatalf("Outbound writes = %v, want request_id then user_id", got)
	}

	// 已存在不覆盖：set 返回 false 时不计入 added。
	if added := Outbound(ctx, func(string, string) bool { return false }); added {
		t.Fatal("Outbound = true when set rejected all keys")
	}

	// 无属性：set 不被调用。
	called := false
	if added := Outbound(context.Background(), func(string, string) bool { called = true; return true }); added || called {
		t.Fatalf("Outbound(empty ctx) added=%v called=%v, want false/false", added, called)
	}

	// nil set：安全返回 false。
	if Outbound(ctx, nil) {
		t.Fatal("Outbound(nil set) = true, want false")
	}
}
