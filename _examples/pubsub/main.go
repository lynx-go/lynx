package main

import (
	"context"
	gohttp "net/http"
	"os"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/kafka"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/lynx-go/x/log"
	"github.com/samber/lo"
)

func main() {
	builder := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))

		// kafka transport 由 kafka.NewFromConfig 从 config.yaml 的 kafka 段加载
		//（--config 指定路径）；段缺失/为空时返回 nil，表示 Kafka 未启用。
		kafkaT, err := kafka.NewFromConfig(app.Config())
		if err != nil {
			return err
		}
		transports := map[string]pubsub.Transport{}
		if kafkaT != nil {
			transports["kafka"] = kafkaT
		}
		// pubsub.NewFromConfig 从 config.yaml 的 pubsub 段加载显式路由并应用
		// RouteKey（逻辑 topic → {transport, key}）；内置内存 Transport 作为
		// 默认回退。路由引用未知 transport 标识（或 kafka 未启用仍引用
		// kafka）时构建期报错，避免路由表静默失真。
		bundle, err := pubsub.NewFromConfig(app.Config(), transports)
		if err != nil {
			return err
		}
		app.Register(bundle.Components()...)
		app.Register(pubsub.NewRouter(bundle.Broker, []pubsub.Handler{&helloHandler{}, &notifyHandler{}}))

		mux := gohttp.NewServeMux()
		mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
			if err := bundle.Broker.Publish(ctx, "hello",
				pubsub.MustJSONMessage(map[string]any{"message": "hello"}),
				pubsub.WithMessageKey(uuid.NewString()),
			); err != nil {
				log.ErrorContext(ctx, "failed to publish", err)
				writer.WriteHeader(gohttp.StatusInternalServerError)
				return
			}
			_, _ = writer.Write([]byte("ok"))
		})
		mux.HandleFunc("/notify", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
			if err := bundle.Broker.Publish(ctx, "notify",
				pubsub.MustJSONMessage(map[string]any{"message": "notify"}),
				pubsub.WithMessageKey(uuid.NewString()),
			); err != nil {
				log.ErrorContext(ctx, "failed to publish", err)
				writer.WriteHeader(gohttp.StatusInternalServerError)
				return
			}
			_, _ = writer.Write([]byte("ok"))
		})
		hs := http.NewServer(mux, http.WithAddr(":7071"))
		app.Register(hs)

		return nil
	},
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)
	builder.Run()
}

type helloHandler struct{}

func (h *helloHandler) EventName() string   { return "hello" }
func (h *helloHandler) HandlerName() string { return "helloHandler" }

func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		log.InfoContext(ctx, "hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)

// notifyHandler 订阅 notify 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到内存 transport，不经过 Kafka）。
type notifyHandler struct{}

func (h *notifyHandler) EventName() string   { return "notify" }
func (h *notifyHandler) HandlerName() string { return "notifyHandler" }

func (h *notifyHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		log.InfoContext(ctx, "notify event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(notifyHandler)
