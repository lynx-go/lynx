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
		app.SetLogger(zap.NewLogger(app))
		broker := pubsub.NewBroker(pubsub.Options{})
		binder := kafka.NewBinder(kafka.BinderOptions{
			SubscribeOptions: map[string]kafka.ConsumerOptions{
				"hello": {
					Brokers: []string{"127.0.0.1:9092"},
					Topic:   "topic_hello",
					Group:   "consumer_hello",
					ErrorHandlerFunc: func(err error) error {
						log.ErrorContext(ctx, "failed to handle event", err)
						return nil
					},
					Instances: 3,
				},
			},
			PublishOptions: map[string]kafka.ProducerOptions{
				"hello": {
					Brokers: []string{"127.0.0.1:9092"},
					Topic:   "topic_hello",
				},
			},
		}, broker)
		if err := app.Hook(lynx.WithComponent(broker, binder)); err != nil {
			return err
		}
		if err := app.Hook(lynx.WithComponentBuilder(binder.Builders()...)); err != nil {
			return err
		}
		router := pubsub.NewRouter(broker, []pubsub.Handler{
			&helloHandler{},
		})
		if err := app.Hook(lynx.WithComponent(router)); err != nil {
			return err
		}
		mux := gohttp.NewServeMux()
		mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
			_ = broker.Publish(ctx, kafka.ToProducerName("hello"), pubsub.NewJSONMessage(map[string]any{"message": "hello"}), pubsub.WithMessageKey(uuid.NewString()))
			_, _ = writer.Write([]byte("ok"))
		})
		hs := http.NewServer(mux, http.WithAddr(":9099"))
		if err := app.Hook(lynx.WithComponent(hs)); err != nil {
			return err
		}

		return nil
	})
	cli.Run()
}

type helloHandler struct {
}

func (h *helloHandler) EventName() string {
	return kafka.ToConsumerName("hello")
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
