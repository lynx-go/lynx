package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"log"
	"time"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	op := lynx.BindOptions()
	op.Name = "cli-example"
	op.Version = "v0.0.1"
	app := lynx.New(op, func(ctx context.Context, lx lynx.Lynx) error {
		config := &Config{}
		if err := lx.Config().Unmarshal(config); err != nil {
			return err
		}

		opt := lx.Option()
		logger := lx.Logger()
		logger.Info("parsed option", "option", opt.String())
		logger.Info("parsed config", "config", config)

		lx.Hooks().OnStart(func(ctx context.Context) error {
			lx.Logger().Info("on start")
			return nil
		})

		lx.Hooks().OnStop(func(ctx context.Context) error {
			lx.Logger().Info("on stop")
			return nil
		})

		if err := lx.MainCommand(func(ctx context.Context) error {
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
