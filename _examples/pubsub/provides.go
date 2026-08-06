package main

import (
	gohttp "net/http"

	"github.com/google/uuid"
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/lynx-go/lynx/contrib/kafka"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/server/http"
	"github.com/lynx-go/x/log"
)

//go:generate wire

// ProviderSet 是 pubsub 示例的 Wire 依赖集合：kafka/pubsub 配置驱动
// 构造函数直接作为 provider（纯函数，Wire 按类型图注入）。
var ProviderSet = wire.NewSet(
	boot.New,
	NewConfig,
	NewKafkaTransport,
	NewBundle,
	NewHandlers,
	NewRouter,
	NewHttpServer,
	NewComponents,
	NewComponentBuilders,
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

// NewBundle 装配消息组件：pubsub.NewFromConfig 从配置 pubsub 段加载
// 显式路由，内置内存 Transport 兜底；kafka 未启用时过滤。
func NewBundle(cfg lynx.Config, kafkaT *kafka.Transport) (*pubsub.Bundle, error) {
	transports := map[string]pubsub.Transport{}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	return pubsub.NewFromConfig(cfg, transports)
}

// NewHandlers 提供事件处理器集合。
func NewHandlers() []pubsub.Handler {
	return []pubsub.Handler{&helloHandler{}, &notifyHandler{}}
}

// NewRouter 将处理器缓冲订阅到 Broker。
func NewRouter(bundle *pubsub.Bundle, handlers []pubsub.Handler) *pubsub.Router {
	return pubsub.NewRouter(bundle.Broker, handlers)
}

// NewHttpServer 构建 HTTP 服务：/hello 与 /notify 端点发布事件。
func NewHttpServer(bundle *pubsub.Bundle) *http.Server {
	mux := gohttp.NewServeMux()
	mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := bundle.Broker.Publish(request.Context(), "hello",
			pubsub.MustJSONMessage(map[string]any{"message": "hello"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			log.ErrorContext(request.Context(), "failed to publish", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	mux.HandleFunc("/notify", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := bundle.Broker.Publish(request.Context(), "notify",
			pubsub.MustJSONMessage(map[string]any{"message": "notify"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			log.ErrorContext(request.Context(), "failed to publish", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	return http.NewServer(mux, http.WithAddr(":7071"))
}

// NewComponents 聚合全部组件供 bootstrap 注册。
func NewComponents(bundle *pubsub.Bundle, router *pubsub.Router, hs *http.Server) []lynx.Component {
	return append(bundle.Components(), router, hs)
}

// NewComponentBuilders 提供空组件构建器集合（pubsub 示例无需动态构建）。
func NewComponentBuilders() []lynx.ComponentBuilder {
	return []lynx.ComponentBuilder{}
}

func NewOnStarts() boot.OnStartHooks { return nil }
func NewOnStops() boot.OnStopHooks   { return nil }
