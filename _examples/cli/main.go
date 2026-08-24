package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/eventbus"
)

type Config struct {
	Addr string `json:"addr"`
}

// HelloTopic 是 CLI 示例的类型化主题（默认内存 Bus）。
var HelloTopic = eventbus.NewTopic[map[string]any]("hello")

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {

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

		// 默认内存 Bus 已由框架注入；Init 期订阅即可。
		if err := HelloTopic.Subscribe(app.Context(),
			func(ctx context.Context, e *eventbus.Event[map[string]any]) error {
				slog.InfoContext(ctx, "recv hello event", "payload", e.Payload)
				return nil
			}); err != nil {
			return err
		}

		fmt.Println("hello cli")

		return app.Command(func(ctx context.Context) error {
			if err := HelloTopic.Publish(ctx, map[string]any{"message": "hello world"}); err != nil {
				return err
			}
			logger.Info("command executed successfully")
			return nil
		})
	},
		lynx.WithName("cli-example"),
	)
	runner.Run()
}
