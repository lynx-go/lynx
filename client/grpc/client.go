// Package grpc 提供框架的 gRPC 客户端：OpenTelemetry 插装
// （otelgrpc client stats handler）、trace 与日志属性
// （request_id/user_id）传播（写入 outgoing metadata）、默认调用超时。
// 与 server/grpc 相对，面向服务间调用场景。
package grpc

import (
	"context"
	"crypto/tls"
	"sync"
	"time"

	"github.com/lynx-go/lynx/logging"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// DefaultTimeout 是默认调用超时（30s）。
const DefaultTimeout = 30 * time.Second

// Options 是 gRPC 客户端的配置项。
type Options struct {
	// Timeout 为 per-RPC 调用超时，0 表示不注入超时。
	Timeout time.Duration
	// TLSConfig 非 nil 时启用 TLS 传输（credentials.NewTLS），与
	// server/grpc 侧 WithTLSConfig 语义对齐。
	TLSConfig *tls.Config
	// TracerProvider 为插装使用的 TracerProvider，nil 时用全局（缺省 noop）。
	TracerProvider trace.TracerProvider
	// DialOptions 透传额外的 grpc.DialOption（如消息大小限制、keepalive）。
	DialOptions []grpc.DialOption
}

// Option 用于配置 gRPC 客户端 Options 的选项函数。
type Option func(*Options)

// WithTimeout 设置默认调用超时（per-RPC context deadline），缺省 30s；
// 调用方 ctx 已带 deadline 时不叠加，以调用方为准。传 0 表示不注入超时。
func WithTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.Timeout = d
	}
}

// WithTLSConfig 启用 TLS 传输，cfg 须已装配证书（tls.LoadX509KeyPair 等）。
// 与 WithDialOptions(grpc.WithTransportCredentials(...)) 同时使用时
// TLSConfig 优先：grpc 对重复凭据取最后应用者，TLSConfig 的凭据装配在
// DialOptions 之后，因此覆盖后者。两者同传属误用，仅以 TLSConfig 为准。
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *Options) {
		o.TLSConfig = cfg
	}
}

// WithTracerProvider 设置客户端插装使用的 TracerProvider；nil 时使用
// 全局（缺省 noop）provider。provider 生命周期由调用方负责，
// 显式注入、不修改进程全局。
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

// WithDialOptions 透传额外的 grpc.DialOption，在内部选项之后应用。
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(o *Options) {
		o.DialOptions = append(o.DialOptions, opts...)
	}
}

// Dial 创建 gRPC 客户端连接，包装 grpc.NewClient：不发起连接（惰性），
// 首次 RPC 时才建立，返回 nil error 不代表对端可达。已装配：
//
//   - 传播：unary/stream 拦截器把 ctx 的日志属性（request_id/user_id，
//     logging.WithAttrs 预置）写入 outgoing metadata（key 与日志字段
//     同名），已存在的 metadata key 不覆盖；otelgrpc stats handler
//     同时注入 trace 传播上下文。
//   - 超时：WithTimeout 的调用超时（缺省 30s）在 RPC 发起时注入 ctx
//     deadline。
//   - 传输：未配置 WithTLSConfig 时使用明文凭据
//     （insecure.NewCredentials，grpc.NewClient 不再隐式缺省）。
//
// 传播边界：服务端（server/grpc）当前不把 incoming metadata 中的
// request_id/user_id 还原为日志属性（v1.1 只做客户端写入，服务端还原
// 入 v1.2 backlog），gRPC 链路暂未形成 HTTP 侧的 request_id 闭环。
func Dial(target string, opts ...Option) (*grpc.ClientConn, error) {
	options := Options{Timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&options)
	}

	otelOpts := []otelgrpc.Option{}
	if options.TracerProvider != nil {
		otelOpts = append(otelOpts, otelgrpc.WithTracerProvider(options.TracerProvider))
	}
	grpcOpts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(otelOpts...)),
		grpc.WithChainUnaryInterceptor(unaryClientInterceptor(options.Timeout)),
		grpc.WithChainStreamInterceptor(streamClientInterceptor(options.Timeout)),
	}
	grpcOpts = append(grpcOpts, options.DialOptions...)
	// TLSConfig 装配在 DialOptions 之后：grpc 对重复凭据取最后应用者，
	// 保证 TLSConfig 优先（见 WithTLSConfig）。未配置 TLS 时显式使用
	// 明文凭据（grpc.NewClient 不再隐式缺省）。
	if options.TLSConfig != nil {
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(credentials.NewTLS(options.TLSConfig)))
	} else {
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return grpc.NewClient(target, grpcOpts...)
}

// unaryClientInterceptor 注入默认调用超时与日志属性传播。
func unaryClientInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, cancel := applyDefaults(ctx, timeout)
		if cancel != nil {
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// streamClientInterceptor 注入默认调用超时与日志属性传播（流创建时
// 写入 outgoing metadata）。超时定时器在流结束（RecvMsg 返回错误/EOF、
// SendMsg/CloseSend 出错）时释放，不存活到 deadline 之后。
func streamClientInterceptor(timeout time.Duration) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx, cancel := applyDefaults(ctx, timeout)
		if cancel == nil {
			return streamer(ctx, desc, cc, method, opts...)
		}
		sc, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			cancel()
			return nil, err
		}
		return &cancelOnEndStream{ClientStream: sc, cancel: cancel}, nil
	}
}

// cancelOnEndStream 在流结束时释放超时定时器（见 streamClientInterceptor）。
type cancelOnEndStream struct {
	grpc.ClientStream
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelOnEndStream) cancelOnce() { s.once.Do(s.cancel) }

func (s *cancelOnEndStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.cancelOnce()
	}
	return err
}

func (s *cancelOnEndStream) SendMsg(m any) error {
	err := s.ClientStream.SendMsg(m)
	if err != nil {
		s.cancelOnce()
	}
	return err
}

func (s *cancelOnEndStream) CloseSend() error {
	err := s.ClientStream.CloseSend()
	if err != nil {
		s.cancelOnce()
	}
	return err
}

// applyDefaults 注入默认调用超时（调用方已有 deadline 时不叠加）并把
// ctx 的日志属性写入 outgoing metadata（已存在的 key 不覆盖）。
// 返回的 cancel 为 nil 表示未注入超时，无需释放。
func applyDefaults(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	var cancel context.CancelFunc
	if timeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			ctx, cancel = context.WithTimeout(ctx, timeout)
		}
	}
	return injectAttrs(ctx), cancel
}

// injectAttrs 把 ctx 的日志属性（request_id/user_id）写入 outgoing
// metadata，key 与日志字段同名；调用方已显式设置的 key 不被覆盖。
func injectAttrs(ctx context.Context) context.Context {
	attrs := logging.AttrsFrom(ctx)
	if len(attrs) == 0 {
		return ctx
	}
	existing, _ := metadata.FromOutgoingContext(ctx)
	md := metadata.New(nil)
	added := false
	for _, a := range attrs {
		if a.Key != logging.FieldRequestID && a.Key != logging.FieldUserID {
			continue
		}
		if len(existing.Get(a.Key)) > 0 {
			continue
		}
		md.Set(a.Key, a.Value.String())
		added = true
	}
	if !added {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, metadata.Join(existing, md))
}
