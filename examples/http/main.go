package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/server/http"
	"log"
	gohttp "net/http"
	"time"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opt := lynx.BindOptions()
	opt.Name = "http-example"
	opt.Version = "v0.0.1"
	app := lynx.New(opt, func(ctx context.Context, lx lynx.Lynx) error {
		config := &Config{}
		if err := lx.Config().Unmarshal(config); err != nil {
			return err
		}
		opt := lx.Option()
		logger := lx.Logger()
		logger.Info("parsed option", "option", opt)
		logger.Info("parsed config", "config", config)

		lx.Hooks().OnStart(func(ctx context.Context) error {
			lx.Logger().Info("on start")
			return nil
		})

		lx.Hooks().OnStop(func(ctx context.Context) error {
			lx.Logger().Info("on stop")
			return nil
		})
		router := http.NewRouter()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			_, _ = rw.Write([]byte("hello"))
		})

		if err := lx.Load(http.NewServer(":9090", router, lx.HealthCheck(), lx.Logger("logger", "http-requestlog"))); err != nil {
			return err
		}

		lx.Hooks().OnStart(func(ctx context.Context) error {
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
