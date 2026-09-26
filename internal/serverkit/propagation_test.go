package serverkit

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lynx-go/lynx/logging"
)

// TestValidPropagationValue 锁定 SC-22 的入站值校验：合法值（UUID/常见
// 追踪 ID）通过，超长与非法字符被拒。
func TestValidPropagationValue(t *testing.T) {
	valid := []string{
		"rid-from-client",
		"0123456789abcdef",
		"aBcD-_1234",
		strings.Repeat("a", MaxPropagationValueLength),
	}
	for _, v := range valid {
		if !ValidPropagationValue(v) {
			t.Errorf("ValidPropagationValue(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", MaxPropagationValueLength+1), // 超长
		"rid with space",
		"rid\twith\ttab",
		"rid<>script", // 注入载荷
		"中文rid",
	}
	for _, v := range invalid {
		if ValidPropagationValue(v) {
			t.Errorf("ValidPropagationValue(%q) = true, want false", v)
		}
	}
}

// TestAttrsFromValues：合法值构造日志属性；非法/空值跳过对应字段。
func TestAttrsFromValues(t *testing.T) {
	attrs := AttrsFromValues("rid-1", "user-1")
	if len(attrs) != 2 {
		t.Fatalf("attrs = %v, want 2", attrs)
	}
	if attrs[0].Key != logging.FieldRequestID || attrs[0].Value.String() != "rid-1" {
		t.Errorf("attrs[0] = %v, want request_id=rid-1", attrs[0])
	}
	if attrs[1].Key != logging.FieldUserID || attrs[1].Value.String() != "user-1" {
		t.Errorf("attrs[1] = %v, want user_id=user-1", attrs[1])
	}

	partial := AttrsFromValues("rid-1", "bad user")
	if len(partial) != 1 || partial[0].Key != logging.FieldRequestID {
		t.Errorf("partial = %v, want only request_id", partial)
	}
	if got := AttrsFromValues("", ""); len(got) != 0 {
		t.Errorf("empty values produced attrs: %v", got)
	}
}

// TestResolveInbound：合法 request_id 沿用；缺失/非法生成新值（返回给
// 调用方回写）；user_id 非法丢弃（不生成）。
func TestResolveInbound(t *testing.T) {
	rid, attrs := ResolveInbound("rid-1", "user-1")
	if rid != "rid-1" {
		t.Errorf("ResolveInbound(valid) rid = %q, want rid-1", rid)
	}
	if len(attrs) != 2 {
		t.Fatalf("attrs = %v, want 2", attrs)
	}

	rid, attrs = ResolveInbound("bad id", "bad user")
	if !ValidPropagationValue(rid) || rid == "bad id" {
		t.Errorf("invalid request_id not regenerated: %q", rid)
	}
	if len(attrs) != 1 || attrs[0].Key != logging.FieldRequestID {
		t.Errorf("attrs = %v, want only regenerated request_id", attrs)
	}

	rid, attrs = ResolveInbound("", "")
	if !ValidPropagationValue(rid) {
		t.Errorf("missing request_id not generated: %q", rid)
	}
	if len(attrs) != 1 || attrs[0].Key != logging.FieldRequestID {
		t.Errorf("attrs = %v, want only request_id (user_id is never generated)", attrs)
	}
}

// TestRequestIDFrom：注入后取值，未注入为空串。
func TestRequestIDFrom(t *testing.T) {
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Errorf("RequestIDFrom(plain ctx) = %q, want empty", got)
	}
	ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "rid-9"))
	if got := RequestIDFrom(ctx); got != "rid-9" {
		t.Errorf("RequestIDFrom(ctx) = %q, want rid-9", got)
	}
}
