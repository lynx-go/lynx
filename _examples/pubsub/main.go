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
		app.Register(memT)
		app.Register(kafkaT)
		app.Register(broker)
		app.Register(pubsub.NewRouter(broker, []pubsub.Handler{&helloHandler{}}))

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
