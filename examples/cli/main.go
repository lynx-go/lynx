package main

import (
	"context"
	"log"
	"time"

	"github.com/lynx-go/lynx"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	op := lynx.ParseFlags()
	op.Name = "cli-example"
	op.Version = "v0.0.1"
	app := lynx.New(op, func(ctx context.Context, app lynx.Lynx) error {
		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		opt := app.Option()
		logger := app.Logger()
		logger.Info("parsed option", "option", opt.String())
		logger.Info("parsed config", "config", config)

		app.Hooks().OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})

		app.Hooks().OnStop(func(ctx context.Context) error {
			app.Logger().Info("on stop")
			return nil
		})

		if err := app.CLI(func(ctx context.Context) error {
			logger.Info("command executed successfully")
			time.Sleep(1 * time.Second)
			return nil
		}); err != nil {
			return err
		}

		return nil
	})
	err := app.RunE()
	if err != nil {
		log.Fatal(err)
	}
}
