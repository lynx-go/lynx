package main

import (
	"context"
	"log"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/http"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opt := lynx.ParseFlags()
	opt.Name = "http-example"
	opt.Version = "v0.0.1"
	app := lynx.New(opt, func(ctx context.Context, app lynx.Lynx) error {
		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}
		opt := app.Option()
		logger := app.Logger()
		logger.Info("parsed option", "option", opt)
		logger.Info("parsed config", "config", config)

		app.Hooks().OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})

		app.Hooks().OnStop(func(ctx context.Context) error {
			app.Logger().Info("on stop")
			return nil
		})
		router := http.NewRouter()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			_, _ = rw.Write([]byte("hello"))
		})

		if err := app.Inject(http.NewServer(":9090", router, app.HealthCheckFunc(), app.Logger("logger", "http-requestlog"))); err != nil {
			return err
		}

		app.Hooks().OnStart(func(ctx context.Context) error {
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
