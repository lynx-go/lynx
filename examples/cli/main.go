package main

import (
	"context"
	"log"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opts := lynx.NewOptions(
		lynx.WithName("cli-example"),
		lynx.WithSetFlags(func(fs *pflag.FlagSet) {
			fs.StringP("config", "c", "./configs", "config file path")
			fs.StringP("loglevel", "l", "debug", "log level")
		}),
		lynx.WithLoadConfig(func(c *viper.Viper) error {
			c.SetEnvPrefix("LYNX")
			c.AutomaticEnv()
			return nil
		}),
	)

	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		opt := app.Option()
		logger := app.Logger()
		logger.Info("parsed option", "option", opt.String())
		logger.Info("parsed config", "config", config)

		app.OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})

		app.OnStop(func(ctx context.Context) error {
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
