package main

import (
	"context"
	"log"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
"
)
type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opts := lynx.NewOptions(
		lynx.WithName("cli-example"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)

	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.NewLogger(app))

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
		logger.Info("parsed config", "config", config)

		app.OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})

		app.OnStop(func(ctx context.Context) error {
			app.Logger().Info("on stop")
			return nil
		})

		return app.CLI(func(ctx context.Context) error {
			logger.Info("command executed successfully")
			time.Sleep(1 * time.Second)
			return nil
		})
	})
	err := app.RunE()
	if err != nil {
		log.Fatal(err)
	}
}
