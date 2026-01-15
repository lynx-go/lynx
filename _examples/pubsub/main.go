package main

import (
	"context"
	gohttp "net/http"
	"os"

	"github.com/ThreeDotsLabs/watermill/message"
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
	options := lynx.NewOptions(
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
		//lynx.WithUseDefaultConfigFlagsFunc(),
	)

	cli := lynx.New(options, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.MustNewLogger(app))
		binder := kafka.NewBinder(kafka.BinderOptions{
			SubscribeOptions: map[string]kafka.ConsumerOptions{
				"hello": {
					Brokers: []string{"127.0.0.1:19092"},
					Topic:   "topic_hello",
					Group:   "consumer_hello",
					ErrorCallbackFunc: func(err error) {
						log.ErrorContext(ctx, "failed to handle event", err)
					},
					Instances:   3,
					MappedEvent: "hello",
					LogMessage:  true,
				},
			},
			PublishOptions: map[string]kafka.ProducerOptions{
				"hello": {
					Brokers:     []string{"127.0.0.1:19092"},
					Topic:       "topic_hello",
					MappedEvent: "hello",
					LogMessage:  true,
				},
			},
		})
		broker := pubsub.NewBroker(pubsub.Options{}, []pubsub.Binder{binder})
		if err := app.Hooks(lynx.Components(broker)); err != nil {
			return err
		}
		if err := app.Hooks(lynx.Components(binder)); err != nil {
			return err
		}
		// 因为 binder 中需要先在 Init() 中初始化 consumer builders，所以 binder.ConsumerBuilders() 不能和 binder 同时注入
		if err := app.Hooks(lynx.ComponentBuilders(binder.ConsumerBuilders()...)); err != nil {
			return err
		}
		router := pubsub.NewRouter(broker, []pubsub.Handler{
			&helloHandler{},
		})
		if err := app.Hooks(lynx.Components(router)); err != nil {
			return err
		}
		mux := gohttp.NewServeMux()
		mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
			_ = broker.Publish(ctx, "hello", pubsub.NewJSONMessage(map[string]any{"message": "hello"}), pubsub.WithMessageKey(uuid.NewString()))
			_, _ = writer.Write([]byte("ok"))
		})
		hs := http.NewServer(mux, http.WithAddr(":7071"))
		if err := app.Hooks(lynx.Components(hs)); err != nil {
			return err
		}

		return nil
	})
	cli.Run()
}

type helloHandler struct {
}

func (h *helloHandler) EventName() string {
	return "hello"
}

func (h *helloHandler) HandlerName() string {
	return "helloHandler"
}

func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *message.Message) error {
		log.InfoContext(ctx, "hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)
