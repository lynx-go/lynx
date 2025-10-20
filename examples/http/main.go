package main

import (
	"context"
	"log"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/log/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/spf13/pflag"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opts := lynx.NewOptions(lynx.WithSetFlags(func(f *pflag.FlagSet) {
		f.StringP("config", "c", "./configs", "config file path")
		f.String("addr", ":8080", "http listen address")
		f.StringP("loglevel", "l", "debug", "log level")
	}))

	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.NewLogger(app, app.Config().GetString("loglevel")))

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}
		opt := app.Option()
		logger := app.Logger()
		logger.Info("parsed option", "option", opt)
		logger.Info("parsed config", "config", config)

		app.OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})

		app.OnStop(func(ctx context.Context) error {
			app.Logger().Info("on stop")
			return nil
		})
		router := http.NewRouter()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			_, _ = rw.Write([]byte("hello"))
		})

		addr := app.Config().GetString("addr")
		if err := app.Hook(http.NewServer(addr, router, app.HealthCheckFunc(), app.Logger("logger", "http-requestlog"))); err != nil {
			return err
		}

		app.OnStart(func(ctx context.Context) error {
			time.Sleep(1 * time.Second)
			return nil
		})

		return nil
	})
	err := app.RunE()
	if err != nil {
		log.Fatal(err)
	}
}
