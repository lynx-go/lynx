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

func TestRequestIDPropagationInvalidValues(t *testing.T) {
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
			// 非法 request_id 重新生成（与 HTTP 侧一致）；user_id 丢弃。
			if got := attrValue(attrs, logging.FieldRequestID); !serverkit.ValidPropagationValue(got) {
				t.Errorf("%s/%s: invalid request_id not regenerated: %q", name, kind, got)
			}
			if got := attrValue(attrs, logging.FieldUserID); got != "" {
				t.Errorf("%s/%s: invalid user_id leaked: %q", name, kind, got)
			}
		}
	}
}

func TestRequestIDPropagationGeneratesWithoutMetadata(t *testing.T) {
	f := runPropagation(context.Background())
	unary := attrValue(f.unaryAttrs, logging.FieldRequestID)
	stream := attrValue(f.streamAttrs, logging.FieldRequestID)
	if !serverkit.ValidPropagationValue(unary) || !serverkit.ValidPropagationValue(stream) {
		t.Fatalf("generated request_ids = %q/%q, want valid values", unary, stream)
	}
	if got := attrValue(f.unaryAttrs, logging.FieldUserID); got != "" {
		t.Errorf("user_id must not be generated, got %q", got)
	}
}

// headerCaptureStream 捕获 SetHeader 的 ServerTransportStream 桩（回写断言用）。
type headerCaptureStream struct {
	md metadata.MD
}

func (s *headerCaptureStream) Method() string { return "/test/Svc" }
func (s *headerCaptureStream) SetHeader(md metadata.MD) error {
	s.md = metadata.Join(s.md, md)
	return nil
}
func (s *headerCaptureStream) SendHeader(md metadata.MD) error { return s.SetHeader(md) }
func (s *headerCaptureStream) SetTrailer(metadata.MD) error    { return nil }

// TestRequestIDPropagationGeneratesAndEchoes：无 incoming metadata 时生成
// request_id，并把解析结果回写响应 metadata（与 HTTP 回写响应头对称）。
func TestRequestIDPropagationGeneratesAndEchoes(t *testing.T) {
	cap := &headerCaptureStream{}
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), cap)
	var attrs []slog.Attr
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Svc"}
	_, _ = RequestIDPropagation()(ctx, nil, info,
		func(ctx context.Context, _ any) (any, error) {
			attrs = logging.AttrsFrom(ctx)
			return nil, nil
		})
	rid := attrValue(attrs, logging.FieldRequestID)
	if !serverkit.ValidPropagationValue(rid) {
		t.Fatalf("generated request_id = %q, want valid value", rid)
	}
	if got := cap.md.Get(serverkit.RequestIDKey); len(got) != 1 || got[0] != rid {
		t.Fatalf("echoed metadata = %v, want [%s]", got, rid)
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
