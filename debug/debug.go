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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/internal/serverkit"
)

const (
	// DefaultAddr 是 debug 服务的缺省监听地址：仅本机回环。
	DefaultAddr = "127.0.0.1:6060"
	// DefaultShutdownTimeout 是优雅关停的上限（可经 WithShutdownTimeout
	// 调整），超过后强制关闭活动连接。pprof 的 profile/trace 端点可长时
	// 运行（秒级采样），3s 对排空在途请求足够。
	DefaultShutdownTimeout = 3 * time.Second
)

// Options 是 debug 服务（pprof）的配置项。
type Options struct {
	Addr   string
	Logger *slog.Logger
	// ShutdownTimeout 是优雅关停的上限；0 表示无配置上界（仅受调用方
	// Stop ctx 约束），与调用方 deadline 并存时取较小者（与 HTTP/gRPC
	// 侧同一规则，共享实现见 internal/serverkit）。
	ShutdownTimeout time.Duration
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

// WithShutdownTimeout 设置优雅关停的上限（缺省 DefaultShutdownTimeout=3s）：
// 与调用方 ctx deadline 并存时取较小者；传 0 表示无配置上界。
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.ShutdownTimeout = timeout
	}
}

// NewService 创建 debug 运维诊断服务，挂载 pprof 端点。
func NewService(opts ...Option) *Service {
	options := Options{
		Addr:            DefaultAddr,
		Logger:          slog.Default(),
		ShutdownTimeout: DefaultShutdownTimeout,
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
	// meta 是 Init 时捕获的应用元数据（name/id/version，来自
	// lynx.Meta），/version 端点输出；ctx 为 nil 时为零值。
	meta lynx.Metadata
	// logLevelCtrl 非 nil（AppContext 实现 SetLogLevel/LogLevel）时
	// /loglevel 端点可用；自定义 logger（SetLogger 定制）的 App 不实现，
	// 端点返回 501。
	logLevelCtrl interface {
		SetLogLevel(slog.Level) bool
		LogLevel() slog.Level
	}
	// stopping 标记 Stop 已被调用：Start 在监听前与监听后各检查一次，
	// 避免 Stop 先于 Start 时留下无人关停的 http.Server。
	stopping atomic.Bool
	// bus 是 Init 时捕获的应用总线（可为 nil，脱离框架单用时）：生命周期
	// 事件经它发布（与 HTTP/gRPC 侧同一组主题，Service="debug"）。
	bus       eventbus.Bus
	ready     chan struct{}
	readyOnce sync.Once
}

// Name 返回服务名称 "debug"。
func (s *Service) Name() string {
	return "debug"
}

// Init 记录日志实例：未显式 WithLogger 时取 ctx.Logger（带服务标签）。
// ctx 为 nil（脱离框架单用）时保持 NewService 的默认 logger。
// 同时捕获应用元数据（/version）与日志级别控制能力（/loglevel，
// AppContext 未实现时该端点返回 501）。
func (s *Service) Init(ctx lynx.AppContext) error {
	if ctx == nil {
		return nil
	}
	if !s.o.loggerSet {
		s.logger = ctx.Logger("service", "debug")
	}
	s.bus = ctx.Bus()
	s.meta = lynx.Meta(ctx.Context())
	s.logLevelCtrl, _ = ctx.(interface {
		SetLogLevel(slog.Level) bool
		LogLevel() slog.Level
	})
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
	srv := &http.Server{Handler: s.newMux()}
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
	serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerListening, "debug", ln.Addr().String(), "")
	s.closeReady()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.ErrorContext(ctx, "debug server serve error", "error", err)
		}
	}()
	// 对齐 lifecycle actor 语义：等待传入的 ctx 取消（框架在 Stop
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
// 有界规则（与 HTTP/gRPC 共用 internal/serverkit）：调用方 ctx deadline
// 与 ShutdownTimeout（缺省 3s）并存时取较小者；超时后强制关闭活动连接
// （采样中的 pprof 请求可能持续数秒），并以错误返回。
func (s *Service) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping debug server", "addr", s.Addr())
	s.stopping.Store(true)
	serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerStopping, "debug", s.Addr(), "")
	s.mu.Lock()
	hs := s.httpServer
	s.httpServer = nil
	// 与 Start 的 ctx.Done 退出路径对称地清掉 listener：两条关停路径
	// 之后 Addr() 一致返回 ""，不残留已关闭监听器的地址（Shutdown 本就
	// 先关 listener，此处置 nil 只是收回查询入口）。
	s.listener = nil
	s.mu.Unlock()
	if hs == nil {
		serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerStopped, "debug", "", "")
		return nil
	}
	defer serverkit.PublishServerEvent(s.logger, s.bus, eventbus.TopicServerStopped, "debug", "", "")
	graceful := func(ctx context.Context) error {
		err := hs.Shutdown(ctx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	return serverkit.Shutdown(ctx, s.o.ShutdownTimeout, s.logger, "debug server", graceful, hs.Close)
}

// newMux 构建自建 mux：显式挂载 pprof handlers，不依赖 net/http/pprof
// 注册到 DefaultServeMux 的全局副作用。除 pprof 外还挂载 /healthz 探活、
// /loglevel 运行时日志级别调整与 /version 构建信息端点。
func (s *Service) newMux() *http.ServeMux {
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
	mux.HandleFunc("/loglevel", s.handleLogLevel)
	mux.HandleFunc("/version", s.handleVersion)
	return mux
}

// handleLogLevel 运行时查询/调整日志级别（运行时可调性；安全边界沿用
// debug 服务整体的本机回环缺省——级别调整不会暴露敏感信息，但放开监听
// 时仍不建议暴露到不受信网络）。
//
// GET /loglevel                       → {"level":"INFO"}
// POST/PUT /loglevel?level=debug      → 调整（query 参数）
// POST/PUT /loglevel  body {"level":"debug"} → 调整（JSON body）
//
// AppContext 未实现级别控制（或用户 SetLogger 定制过 handler）时返回
// 501，GET 返回记账值。非法级别返回 400。
func (s *Service) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		level := slog.LevelInfo
		if s.logLevelCtrl != nil {
			level = s.logLevelCtrl.LogLevel()
		}
		_, _ = fmt.Fprintf(w, `{"level":%q}`, level.String())
	case http.MethodPost, http.MethodPut:
		if s.logLevelCtrl == nil {
			http.Error(w, `{"error":"log level control not available"}`, http.StatusNotImplemented)
			return
		}
		levelStr := r.URL.Query().Get("level")
		if levelStr == "" && r.Body != nil {
			var body struct {
				Level string `json:"level"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err == nil {
				levelStr = body.Level
			}
		}
		level, err := lynx.ParseLogLevel(levelStr)
		if err != nil || levelStr == "" {
			http.Error(w, `{"error":"invalid level"}`, http.StatusBadRequest)
			return
		}
		if !s.logLevelCtrl.SetLogLevel(level) {
			http.Error(w, `{"error":"logger is customized, level not adjustable"}`, http.StatusConflict)
			return
		}
		s.logger.InfoContext(r.Context(), "log level adjusted", "level", level.String())
		_, _ = fmt.Fprintf(w, `{"level":%q}`, level.String())
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleVersion 输出构建信息（ldflags 注入的 BuildVersion/BuildCommit/
// BuildDate，见 vars.go）叠加 Go/OS/Arch 与应用元数据（Init 捕获）。
func (s *Service) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Date    string `json:"date"`
		Go      string `json:"go"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
		Service struct {
			Name    string `json:"name,omitempty"`
			ID      string `json:"id,omitempty"`
			Version string `json:"version,omitempty"`
		} `json:"service"`
	}{
		Version: BuildVersion,
		Commit:  BuildCommit,
		Date:    BuildDate,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Service: struct {
			Name    string `json:"name,omitempty"`
			ID      string `json:"id,omitempty"`
			Version string `json:"version,omitempty"`
		}{Name: s.meta.Name, ID: s.meta.ID, Version: s.meta.Version},
	})
}

var _ lynx.Service = (*Service)(nil)

var _ lynx.Checker = (*Service)(nil)

var _ lynx.Ready = (*Service)(nil)
