// Package debug 提供运维诊断服务：pprof 端点（仅建议监听本机回环地址）。
// Service 实现 lynx.Service：Init/Start/Stop 全生命周期契约，
// Stop 容忍先于 Start 调用，Start 阻塞在传入 ctx。
//
// 安全警示：pprof 端点会暴露进程内存快照、源码路径、环境变量等敏感信息，
// 缺省仅监听本机回环地址（127.0.0.1:6060）。生产环境不得将 pprof 端口
// 暴露到公网或集群外部；如需远程诊断，请使用 SSH 端口转发
// （ssh -L 6060:127.0.0.1:6060 host），不要直接开放监听端口。
package debug

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
)

const (
	// DefaultAddr 是 debug 服务的缺省监听地址：仅本机回环。
	DefaultAddr = "127.0.0.1:6060"
	// shutdownTimeout 是优雅关停的上限，超过后强制关闭活动连接。
	// pprof 的 profile/trace 端点可长时运行（秒级采样），3s 对排空
	// 在途请求足够；与 server/http 不同，此处固定值即可，无需暴露为
	// 可配置项。
	shutdownTimeout = 3 * time.Second
)

// Options 是 debug 服务（pprof）的配置项。
type Options struct {
	Addr   string
	Logger *slog.Logger
	// loggerSet 标记 Logger 是否由 WithLogger 显式设置：显式设置时
	// Init 不再用 ctx.Logger 覆盖。
	loggerSet bool
}

// Option 用于配置 debug 服务 Options 的选项函数。
type Option func(*Options)

// WithAddr 设置 debug 服务监听地址；缺省 DefaultAddr（"127.0.0.1:6060"）。
// 测试可用 "127.0.0.1:0" 取随机端口，Start 后经 Addr() 获取实际地址。
// 安全警示：生产环境不得将 pprof 端口暴露公网/集群外（见包注释）。
func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

// WithLogger 设置 debug 服务的日志实例；缺省 Init 时取 ctx.Logger，
// 再缺省 slog.Default()。
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
		o.loggerSet = true
	}
}

// NewService 创建 debug 运维诊断服务，挂载 pprof 端点。
func NewService(opts ...Option) *Service {
	options := Options{
		Addr:   DefaultAddr,
		Logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(&options)
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Service{
		logger: options.Logger,
		o:      options,
		ready:  make(chan struct{}),
	}
}

// Service 是 debug 运维诊断服务，实现 lynx.Service 接口。
type Service struct {
	// mu 守卫 httpServer 与 listener：Start 写入，Stop 在关停路径上
	// 可能于另一 goroutine 读取。
	mu         sync.RWMutex
	httpServer *http.Server
	listener   net.Listener
	logger     *slog.Logger
	o          Options
	started    atomic.Bool
	// stopping 标记 Stop 已被调用：Start 在监听前与监听后各检查一次，
	// 避免 Stop 先于 Start 时留下无人关停的 http.Server。
	stopping  atomic.Bool
	ready     chan struct{}
	readyOnce sync.Once
}

// Name 返回服务名称 "debug"。
func (s *Service) Name() string {
	return "debug"
}

// Init 记录日志实例：未显式 WithLogger 时取 ctx.Logger（带服务标签）。
// ctx 为 nil（脱离框架单用）时保持 NewService 的默认 logger。
func (s *Service) Init(ctx lynx.AppContext) error {
	if ctx == nil {
		return nil
	}
	if !s.o.loggerSet {
		s.logger = ctx.Logger("service", "debug")
	}
	return nil
}

// Addr 返回当前监听地址：Start 前返回空字符串；使用随机端口
// （如 "127.0.0.1:0"）时返回实际分配的地址，供测试与探活使用。
// Stop 或 Start 的 ctx 取消之后（两条关停路径一致清理）同样返回空字符串。
func (s *Service) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// CheckHealth 实现健康检查，服务未在运行时返回错误。
// 已知窗口：Stop 返回到 Start 退出之间存在短暂交错（框架在 Stop 返回后
// 取消服务 ctx，Start 才从等待中醒来），窗口内 started 尚为 true，
// CheckHealth 可能仍报健康——进程已在关停路径上，不构成误报，行为保持
// 不变（AUX-09 注释化）。
func (s *Service) CheckHealth() error {
	if !s.started.Load() {
		return errors.New("debug server not running")
	}
	return nil
}

// Ready 在 Listen 成功并置位 started 之后关闭。Listen 失败或不启动不关闭。
func (s *Service) Ready() <-chan struct{} {
	return s.ready
}

func (s *Service) closeReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

// Start 启动 pprof HTTP 服务并阻塞至传入 ctx 取消。
// 竞态安全：Stop 先于本方法调用时（服务启动失败引发的提前中断）不启动
// 并立即返回；Stop 恰在本方法监听前后交错时同样收敛（见内注释）。
func (s *Service) Start(ctx context.Context) error {
	if s.stopping.Load() {
		// Stop 先到：不启动，直接返回（Stop-before-Start 契约）。
		return nil
	}
	ln, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: newMux()}
	s.mu.Lock()
	s.httpServer = srv
	s.listener = ln
	s.mu.Unlock()
	if s.stopping.Load() {
		// Stop 恰在此前执行且未拿到 httpServer（其 Shutdown 拿到 nil
		// 直接返回）：此处补发真正关闭，不进入阻塞等待——否则 ctx 永无
		// 人取消，Start 挂死（Stop/Start 交错窗口）。
		s.mu.Lock()
		s.httpServer = nil
		s.listener = nil
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("debug server stopped before start")
	}
	// "started" 日志置于 stopping 复查之后：交错窗口内（Stop 已先到）
	// 不再打出误导性的启动日志（AUX-08）。
	s.logger.InfoContext(ctx, "debug server started", "addr", ln.Addr().String())
	s.started.Store(true)
	s.closeReady()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.ErrorContext(ctx, "debug server serve error", "error", err)
		}
	}()
	// 对齐 run.Group actor 语义：等待传入的 ctx 取消（框架在 Stop
	// 返回后取消服务 ctx）。
	<-ctx.Done()
	s.started.Store(false)
	// 独立使用（脱离框架、未经 Stop 直接取消 ctx）时的唯一退出路径：
	// 与 Stop 的清理对称地关闭 httpServer 释放端口，否则 listener 一直
	// 占用到进程退出。持 mu 与 Stop 互斥：双方中只有一方能拿到 httpServer
	//（另一方读到 nil）；Close 幂等，Stop 已先清理时此处无副作用。
	s.mu.Lock()
	hs := s.httpServer
	s.httpServer = nil
	s.listener = nil
	s.mu.Unlock()
	if hs != nil {
		_ = hs.Close()
	}
	return nil
}

// Stop 优雅关停 pprof 服务；服务尚未启动时直接返回 nil。
// 调用方 context 无 deadline 时使用内置 shutdownTimeout（3s）上限，
// 超时后强制关闭活动连接（采样中的 pprof 请求可能持续数秒），
// 并以错误返回。
func (s *Service) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping debug server", "addr", s.Addr())
	s.stopping.Store(true)
	s.mu.Lock()
	hs := s.httpServer
	s.httpServer = nil
	// 与 Start 的 ctx.Done 退出路径对称地清掉 listener：两条关停路径
	// 之后 Addr() 一致返回 ""，不残留已关闭监听器的地址（Shutdown 本就
	// 先关 listener，此处置 nil 只是收回查询入口）。
	s.listener = nil
	s.mu.Unlock()
	if hs == nil {
		return nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
	}
	done := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(done)
		shutdownErr = hs.Shutdown(ctx)
	}()
	select {
	case <-done:
		switch {
		case shutdownErr == nil, errors.Is(shutdownErr, http.ErrServerClosed):
			// 正常优雅关停。
			return nil
		case errors.Is(shutdownErr, context.Canceled), errors.Is(shutdownErr, context.DeadlineExceeded):
			// Shutdown 因超时/取消返回：与 ctx.Done() 分支同样归入超时
			// 路径，强制关闭活动连接并以错误返回。
			s.logger.ErrorContext(ctx, "graceful debug server shutdown timed out, forcing close", "error", shutdownErr)
			if err := hs.Close(); err != nil {
				s.logger.ErrorContext(ctx, "failed to force-close debug server", "error", err)
			}
			return fmt.Errorf("debug server graceful shutdown timed out: %w", shutdownErr)
		default:
			s.logger.ErrorContext(ctx, "failed to shutdown debug server", "error", shutdownErr)
			return shutdownErr
		}
	case <-ctx.Done():
		// ctx 已结束，Shutdown 必然很快返回；先等它退出再强制关闭，
		// 避免与 Shutdown 内部的连接清理交错。
		<-done
		s.logger.ErrorContext(ctx, "graceful debug server shutdown timed out, forcing close", "error", ctx.Err())
		if err := hs.Close(); err != nil {
			s.logger.ErrorContext(ctx, "failed to force-close debug server", "error", err)
		}
		return fmt.Errorf("debug server graceful shutdown timed out: %w", ctx.Err())
	}
}

// newMux 构建自建 mux：显式挂载 pprof handlers，不依赖 net/http/pprof
// 注册到 DefaultServeMux 的全局副作用。
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range []string{
		"allocs", "block", "goroutine", "heap", "mutex", "threadcreate",
	} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
	// /healthz 便于探活：进程存活即 200。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

var _ lynx.Service = (*Service)(nil)

var _ lynx.Checker = (*Service)(nil)

var _ lynx.Ready = (*Service)(nil)
