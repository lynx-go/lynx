package interceptor

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lynx-go/lynx/internal/serverkit"
	"github.com/lynx-go/lynx/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// requestIDPropagationFixture 驱动一元/流式还原拦截器并收集 handler
// 看到的 ctx 日志属性。
type requestIDPropagationFixture struct {
	unaryAttrs  []slog.Attr
	streamAttrs []slog.Attr
}

func runPropagation(ctx context.Context) *requestIDPropagationFixture {
	f := &requestIDPropagationFixture{}
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Svc"}
	_, _ = RequestIDPropagation()(ctx, nil, info,
		func(ctx context.Context, _ any) (any, error) {
			f.unaryAttrs = logging.AttrsFrom(ctx)
			return nil, nil
		})
	sinfo := &grpc.StreamServerInfo{FullMethod: "/test/Svc"}
	_ = RequestIDPropagationStream()(nil, &stubServerStream{ctx: ctx}, sinfo,
		func(_ any, ss grpc.ServerStream) error {
			f.streamAttrs = logging.AttrsFrom(ss.Context())
			return nil
		})
	return f
}

// attrValue 按 key 取属性值，未命中返回空字符串。
func attrValue(attrs []slog.Attr, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

// stubServerStream 仅承载 ctx 的最小 ServerStream 桩。
type stubServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stubServerStream) Context() context.Context { return s.ctx }

func TestRequestIDPropagationRestoresBothFields(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(serverkit.RequestIDKey, "req-1", serverkit.UserIDKey, "user-9"))
	f := runPropagation(ctx)
	for name, attrs := range map[string][]slog.Attr{"unary": f.unaryAttrs, "stream": f.streamAttrs} {
		if got := attrValue(attrs, logging.FieldRequestID); got != "req-1" {
			t.Errorf("%s request_id = %q, want req-1", name, got)
		}
		if got := attrValue(attrs, logging.FieldUserID); got != "user-9" {
			t.Errorf("%s user_id = %q, want user-9", name, got)
		}
	}
}

func TestRequestIDPropagationDropsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"overlong":      strings.Repeat("a", serverkit.MaxPropagationValueLength+1),
		"illegal chars": "bad id\n<script>",
		"empty":         "",
		"space":         "bad id",
	}
	for name, v := range cases {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(serverkit.RequestIDKey, v, serverkit.UserIDKey, strings.Repeat("é", 5)))
		f := runPropagation(ctx)
		for kind, attrs := range map[string][]slog.Attr{"unary": f.unaryAttrs, "stream": f.streamAttrs} {
			if attrValue(attrs, logging.FieldRequestID) != "" || attrValue(attrs, logging.FieldUserID) != "" {
				t.Errorf("%s/%s: invalid values leaked into %s attrs", name, kind, kind)
			}
		}
	}
}

func TestRequestIDPropagationNoMetadata(t *testing.T) {
	f := runPropagation(context.Background())
	if len(f.unaryAttrs) != 0 || len(f.streamAttrs) != 0 {
		t.Errorf("attrs injected without metadata: unary=%v stream=%v", f.unaryAttrs, f.streamAttrs)
	}
}

// 注入为 WithAttrs 覆盖语义（同 key 最新写入为准，与 HTTP 侧一致）：
// 直接驱动拦截器且 ctx 已有同名属性时，metadata 值生效。
func TestRequestIDPropagationMetadataWins(t *testing.T) {
	ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, "orig"))
	ctx = metadata.NewIncomingContext(ctx,
		metadata.Pairs(serverkit.RequestIDKey, "from-metadata"))
	f := runPropagation(ctx)
	if got := attrValue(f.unaryAttrs, logging.FieldRequestID); got != "from-metadata" {
		t.Errorf("unary request_id = %q, want from-metadata (WithAttrs overwrite semantics)", got)
	}
	if got := attrValue(f.streamAttrs, logging.FieldRequestID); got != "from-metadata" {
		t.Errorf("stream request_id = %q, want from-metadata (WithAttrs overwrite semantics)", got)
	}
}

// 多值 metadata 取首个。
func TestRequestIDPropagationUsesFirstValue(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.MD{serverkit.RequestIDKey: []string{"first", "second"}})
	f := runPropagation(ctx)
	if got := attrValue(f.unaryAttrs, logging.FieldRequestID); got != "first" {
		t.Errorf("request_id = %q, want first", got)
	}
}
