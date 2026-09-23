package serverkit

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// PublishServerEvent 发布 server 生命周期事件（bus 为 nil 时 no-op，
// 与 App.publishEvent 的容错一致）。三个 server 适配器共用同一组主题
// （lynx.server.*），订阅方按 ServerEvent.Service 区分 http/grpc/debug。
func PublishServerEvent(logger *slog.Logger, bus eventbus.Bus, topic, service, addr, advertiseAddr string) {
	if bus == nil {
		return
	}
	ctx := context.Background()
	err := bus.Publish(ctx, topic, eventbus.ServerEvent{
		Service:       service,
		Addr:          addr,
		AdvertiseAddr: advertiseAddr,
		Time:          time.Now(),
	})
	if err != nil && logger != nil {
		logger.DebugContext(ctx, "publish server event failed", "topic", topic, "error", err)
	}
}
