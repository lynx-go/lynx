package main

import (
	"log/slog"
	gohttp "net/http"

	"github.com/google/uuid"
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/lynx-go/lynx/contrib/kafka"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/server/http"
)

//go:generate wire

// ProviderSet 是 pubsub 示例的 Wire 依赖集合：kafka/pubsub 配置驱动
// 构造函数直接作为 provider（纯函数，Wire 按类型图注入）。
var ProviderSet = wire.NewSet(
	boot.New,
	NewConfig,
	NewKafkaTransport,
	NewMemoryTransport,
	NewBroker,
	NewHandlers,
	NewRouter,
	NewHttpServer,
	NewServices,
	NewServiceFactories,
	NewOnStarts,
	NewOnStops,
)

// NewConfig 提供应用配置（kafka/pubsub 段的读取源）。
func NewConfig(app lynx.App) lynx.Config {
	return app.Config()
}

// NewKafkaTransport 从配置 kafka 段创建 Transport；段缺失/为空时
// kafka.NewFromConfig 返回 nil（未启用），Wire 注入 nil 指针。
func NewKafkaTransport(cfg lynx.Config) (*kafka.Transport, error) {
	return kafka.NewFromConfig(cfg)
}

// NewMemoryTransport 提供进程内 Transport（默认回退与本地开发用）。
func NewMemoryTransport() *pubsub.MemoryTransport {
	return pubsub.NewMemoryTransport()
}

// NewBroker 装配消息服务：pubsub.NewFromConfig 从配置 pubsub 段加载
// 显式路由，memory 兼作默认回退；kafka 未启用时过滤。
func NewBroker(cfg lynx.Config, kafkaT *kafka.Transport, memT *pubsub.MemoryTransport) (pubsub.Broker, error) {
	transports := map[string]pubsub.Transport{"memory": memT}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	return pubsub.NewFromConfig(cfg, transports)
}

// NewHandlers 提供事件处理器集合。
func NewHandlers() []pubsub.Handler {
	return []pubsub.Handler{helloHandler(), notifyHandler()}
}

// NewRouter 将处理器缓冲订阅到 Broker。
func NewRouter(broker pubsub.Broker, handlers []pubsub.Handler) *pubsub.Router {
	return pubsub.NewRouter(broker, handlers)
}

// NewHttpServer 构建 HTTP 服务：/hello 与 /notify 端点发布事件。
func NewHttpServer(broker pubsub.Broker) *http.Server {
	mux := gohttp.NewServeMux()
	mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		ctx := request.Context()
		slog.DebugContext(ctx, "Handling /hello request")
		if err := broker.Publish(request.Context(), "hello",
			HelloEvent{Message: "hello"},
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			slog.ErrorContext(request.Context(), "failed to publish", "error", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	mux.HandleFunc("/notify", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := broker.Publish(request.Context(), "notify",
			pubsub.MustJSONMessage(map[string]any{"message": "notify"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			slog.ErrorContext(request.Context(), "failed to publish", "error", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	return http.NewServer(mux, http.WithAddr(":7071"))
}

// NewServices 聚合全部服务供 bootstrap 注册。
func NewServices(memT *pubsub.MemoryTransport, kafkaT *kafka.Transport,
	broker pubsub.Broker, router *pubsub.Router, hs *http.Server) []lynx.Service {
	comps := []lynx.Service{memT}
	if kafkaT != nil {
		comps = append(comps, kafkaT)
	}
	return append(comps, broker, router, hs)
}

// NewServiceFactories 提供空服务工厂集合（pubsub 示例无需动态构建）。
func NewServiceFactories() []lynx.ServiceFactory {
	return []lynx.ServiceFactory{}
}

func NewOnStarts() boot.OnStartHooks { return nil }
func NewOnStops() boot.OnStopHooks   { return nil }
