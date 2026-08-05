package main

import (
	"context"
	"fmt"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/x/log"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	builder := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {

		logLevel := app.Config().GetString("log-level")
		if logLevel == "" {
			logLevel = "debug"
		}
		zlogger, err := zap.NewZapLoggerToFile(logLevel, "cli.out")
		if err != nil {
			return err
		}
		slogger, err := zap.NewSLogger(zlogger, logLevel)
		if err != nil {
			return err
		}
		app.SetLogger(slogger)

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
		logger.Info("parsed config", "config", config)

		broker := pubsub.NewBroker(pubsub.Options{DefaultTransport: pubsub.NewMemoryTransport()})
		app.Register(broker)
		router := pubsub.NewRouter(broker, []pubsub.Handler{
			&helloHandler{},
		})
		app.Register(router)

		fmt.Println("hello cli")

		return app.CLI(func(ctx context.Context) error {
			if err := broker.Publish(ctx, "hello", pubsub.MustJSONMessage(map[string]any{"message": "hello world"})); err != nil {
				return err
			}
			//time.Sleep(1 * time.Second)
			logger.Info("command executed successfully")
			return nil
		})
	},
		lynx.WithName("cli-example"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)
	builder.Run()
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
	return func(ctx context.Context, event *pubsub.Message) error {
		log.InfoContext(ctx, "recv hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)
