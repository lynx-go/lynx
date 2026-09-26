package serverkit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// Lifecycle 是 HTTP / gRPC / debug 三个 server 适配器共享的生命周期状态机
// （唯一归属）：Start 重入守卫、停止请求标志、就绪信号与生命周期事件发布。
// 适配器保留传输细节（listener/server 创建、Serve/GracefulStop、地址策略、
// 健康轮询与启动期交错处理），只把状态迁移委托到这里。
//
// 语义：
//   - BeginStart 二次调用返回错误（带 service 名）；Init 经 ResetForInit
//     复位守卫与停止标志（ready 一旦关闭无法复位，重启语义仅部分）；
//     Listen/端点配置失败经 AbortStart 复位守卫，允许修正后重试。
//   - BeginStop 置位停止请求（幂等）；StopRequested 供 Start 的启动期
//     交错检查（Stop 先于 Start 到达时不进入 Serve）。
//   - MarkReady 关闭就绪信号（once）；Ready 返回只读 channel。
//   - Listening/Stopping/Stopped 发布 lynx.server.* 事件（bus 为 nil 时
//     no-op，发布失败仅 Debug 日志）。
type Lifecycle struct {
	service string

	mu     sync.Mutex
	logger *slog.Logger
	bus    eventbus.Bus

	started       atomic.Bool
	stopRequested atomic.Bool
	ready         chan struct{}
	readyOnce     sync.Once
}

// NewLifecycle 创建生命周期状态机；service 是事件里的 ServerEvent.Service
// （"http" / "grpc" / "debug"），也用于重入错误文案。
func NewLifecycle(service string) *Lifecycle {
	return &Lifecycle{
		service: service,
		ready:   make(chan struct{}),
	}
}

// Bind 注入日志与总线（适配器 Init 时调用；bus 可为 nil——脱离框架单用时
// 事件发布 no-op）。Bind 可重复调用（重新 Init 场景）。
func (l *Lifecycle) Bind(logger *slog.Logger, bus eventbus.Bus) {
	l.mu.Lock()
	l.logger = logger
	l.bus = bus
	l.mu.Unlock()
}

// ResetForInit 复位守卫与停止标志（适配器 Init 调用）：允许重新 Init 后
// 再次 Start。ready 一旦关闭无法复位——重启语义仅部分（就绪信号不重置），
// 与既有 HTTP/gRPC 语义一致。
func (l *Lifecycle) ResetForInit() {
	l.started.Store(false)
	l.stopRequested.Store(false)
}

// BeginStart 是 Start 的重入守卫：二次 Start 返回错误（会覆盖 listener/
// server 并泄漏旧实例）。
func (l *Lifecycle) BeginStart() error {
	if !l.started.CompareAndSwap(false, true) {
		return fmt.Errorf("%s server: Start called more than once", l.service)
	}
	return nil
}

// AbortStart 复位守卫：Listen/端点配置等启动失败不算已启动，允许修正后
// 重试（与 BeginStart 的成功路径对称）。
func (l *Lifecycle) AbortStart() {
	l.started.Store(false)
}

// BeginStop 置位停止请求（幂等）。
func (l *Lifecycle) BeginStop() {
	l.stopRequested.Store(true)
}

// StopRequested 报告 Stop 是否已请求：Start 在进入 Serve 之前检查——已
// 中断后不再 Serve，避免「Stop 见 server 为 nil 先返回、Start 随后监听并
// 永久运行」的启动期交错。
func (l *Lifecycle) StopRequested() bool {
	return l.stopRequested.Load()
}

// MarkReady 关闭就绪信号（幂等：once 语义）。
func (l *Lifecycle) MarkReady() {
	l.readyOnce.Do(func() { close(l.ready) })
}

// Ready 返回就绪信号：MarkReady 之后关闭。
func (l *Lifecycle) Ready() <-chan struct{} {
	return l.ready
}

// Listening 发布 lynx.server.listening 事件。
func (l *Lifecycle) Listening(addr, advertiseAddr string) {
	l.publish(eventbus.TopicServerListening, addr, advertiseAddr)
}

// Stopping 发布 lynx.server.stopping 事件。
func (l *Lifecycle) Stopping(addr, advertiseAddr string) {
	l.publish(eventbus.TopicServerStopping, addr, advertiseAddr)
}

// Stopped 发布 lynx.server.stopped 事件。
func (l *Lifecycle) Stopped() {
	l.publish(eventbus.TopicServerStopped, "", "")
}

// publish 发布 server 生命周期事件（bus 为 nil 时 no-op，与 App.publishEvent
// 的容错一致；发布失败仅 Debug 日志）。
func (l *Lifecycle) publish(topic, addr, advertiseAddr string) {
	l.mu.Lock()
	logger, bus := l.logger, l.bus
	l.mu.Unlock()
	if bus == nil {
		return
	}
	ctx := context.Background()
	err := bus.Publish(ctx, topic, eventbus.ServerEvent{
		Service:       l.service,
		Addr:          addr,
		AdvertiseAddr: advertiseAddr,
		Time:          time.Now(),
	})
	if err != nil && logger != nil {
		logger.DebugContext(ctx, "publish server event failed", "topic", topic, "error", err)
	}
}
