package main

import (
	"context"
	"log"
	gohttp "net/http"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/log/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/spf13/pflag"
)

var (
	id      string
	version string
)

func main() {
	id, _ = os.Hostname()

	opts := lynx.NewOptions(lynx.WithSetFlags(func(f *pflag.FlagSet) {
		f.String("addr", ":8080", "http listen address")
		f.StringP("loglevel", "l", "debug", "log level")
	}))

	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.NewLogger(app, app.Config().GetString("loglevel")))
		boot, cleanup, err := wireBootstrap(app, app.Logger())
		if err != nil {
			log.Fatal(err)
		}
		app.OnStop(func(ctx context.Context) error {
			cleanup()
			return nil
		})
		return boot.Wire(app)
	})
	app.Run()
}

func NewHttpServer(app lynx.Lynx) *http.Server {
	router := http.NewRouter()
	router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
		_, _ = rw.Write([]byte("hello"))
	})
	addr := app.Config().GetString("addr")

	return http.NewServer(addr, router, app.HealthCheckFunc(), app.Logger("logger", "http-requestlog"))
}
