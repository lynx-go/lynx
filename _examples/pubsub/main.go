package main

import (
	"context"
	"fmt"
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

		// kafka 配置从 config.yaml 的 kafka 段加载（--config 指定路径）。
		var kafkaOpts kafka.Options
		if err := app.Config().UnmarshalKey("kafka", &kafkaOpts); err != nil {
			return err
		}
		kafkaT, err := kafka.NewTransport(kafkaOpts)
		if err != nil {
			return err
		}
		memT := pubsub.NewMemoryTransport()
		broker := pubsub.NewBroker(pubsub.Options{
			Transports:       []pubsub.Transport{kafkaT},
			DefaultTransport: memT,
		})

		// 显式路由从 config.yaml 的 pubsub 段加载：逻辑 topic → {transport,
		// key}。key 是调用 transport 时的主题名（kafka 时为 kafka 段配置的
		// key），经 broker.RouteKey 应用，覆盖自动路由；transport 标识未知
		// 时报错，避免路由表静默失真。
		var pubsubCfg struct {
			Routes map[string]struct {
				Transport string
				Key       string
			}
		}
		if err := app.Config().UnmarshalKey("pubsub", &pubsubCfg); err != nil {
			return err
		}
		transports := map[string]pubsub.Transport{
			"kafka":  kafkaT,
			"memory": memT,
		}
		for topic, route := range pubsubCfg.Routes {
			t, ok := transports[route.Transport]
			if !ok {
				return fmt.Errorf("pubsub: route %q references unknown transport %q", topic, route.Transport)
			}
			if route.Key == "" {
				route.Key = topic
			}
			broker.RouteKey(topic, t, route.Key)
		}

		app.Register(memT)
		app.Register(kafkaT)
		app.Register(broker)
		app.Register(pubsub.NewRouter(broker, []pubsub.Handler{&helloHandler{}, &notifyHandler{}}))

		mux := gohttp.NewServeMux()
		mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
			if err := broker.Publish(ctx, "hello",
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
			if err := broker.Publish(ctx, "notify",
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
