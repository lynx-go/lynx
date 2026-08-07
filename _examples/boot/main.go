package main

import (
	"context"
	"log"
	gohttp "net/http"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/spf13/pflag"
)

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))
		boot, cleanup, err := wireBootstrap(app, app.Logger())
		if err != nil {
			log.Fatal(err)
		}
		app.OnStop(func(ctx context.Context) error {
			cleanup()
			return nil
		})
		boot.Bind(app)
		return nil
	},
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.String("addr", ":8080", "http listen address")
			f.StringP("loglevel", "l", "debug", "log level")
			f.StringP("config", "c", "", "config file path")
		}),
		lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
			if cf, _ := f.GetString("config"); cf != "" {
				c.SetFile(cf)
			}
			return nil
		}),
	)
	runner.Run()
}

func NewHttpServer(app lynx.App) *http.Server {
	router := gohttp.NewServeMux()
	router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
		_, _ = rw.Write([]byte("hello"))
	})
	addr := app.Config().GetString("addr")

	return http.NewServer(router, http.WithAddr(addr), http.WithHealthCheckers(app.HealthCheckers), http.WithLogger(app.Logger("logger", "http-requestlog")))
}
