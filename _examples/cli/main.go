package main

import (
	"context"
	"fmt"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/pkg/errors"
	"github.com/lynx-go/x/log"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opts := lynx.NewOptions(
		lynx.WithName("cli-example"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)

	cli := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {

		logLevel := app.Config().GetString("log-level")
		if logLevel == "" {
			logLevel = "debug"
		}
		zlogger, err := zap.NewZapLoggerToFile(logLevel, "cli.out")
		errors.Fatal(err)
		slogger, err := zap.NewSLogger(zlogger, logLevel)
		errors.Fatal(err)
		app.SetLogger(slogger)

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
		logger.Info("parsed config", "config", config)

		broker := pubsub.NewBroker(pubsub.Options{}, nil)
		if err := app.Hooks(lynx.Components(broker)); err != nil {
			return err
		}
		router := pubsub.NewRouter(broker, []pubsub.Handler{
			&helloHandler{},
		})
		if err := app.Hooks(lynx.Components(router)); err != nil {
			return err
		}

		fmt.Println("hello cli")

		return app.CLI(func(ctx context.Context) error {
			if err := broker.Publish(ctx, "hello", pubsub.NewJSONMessage(map[string]any{"message": "hello world"})); err != nil {
				return err
			}
			//time.Sleep(1 * time.Second)
			logger.Info("command executed successfully")
			return nil
		})
	})
	cli.Run()
}

type helloHandler struct {
}

func (h *helloHandler) EventName() string {
	//return kafka.ToConsumerName("hello")
	return "hello"
}

func (h *helloHandler) HandlerName() string {
	return "helloHandler"
}

func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *message.Message) error {
		log.InfoContext(ctx, "recv hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)
