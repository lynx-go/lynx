package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/contrib/zap"
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
		zlogger, err := zap.NewZapLogger(logLevel, "cli.out")
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
			pubsub.NewHandler("hello", "helloHandler", func(ctx context.Context, event *pubsub.Message) error {
				slog.InfoContext(ctx, "recv hello event", "payload", string(event.Payload))
				return nil
			}),
		})
		app.Register(router)

		fmt.Println("hello cli")

		return app.Command(func(ctx context.Context) error {
			if err := broker.Publish(ctx, "hello", pubsub.MustJSONMessage(map[string]any{"message": "hello world"})); err != nil {
				return err
			}
			logger.Info("command executed successfully")
			return nil
		})
	},
		lynx.WithName("cli-example"),
	)
	builder.Run()
}
