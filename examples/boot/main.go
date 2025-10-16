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
)

var (
	id      string
	version string
)

func main() {
	id, _ = os.Hostname()

	op := lynx.ParseFlags()
	op.ID = id
	op.Version = version
	op.Name = "fleet"
	app := lynx.New(op, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.NewLogger(app, app.Option().LogLevel))
		boot, cleanup, err := wireBootstrap(app, app.Option(), app.Config(), app.Logger())
		if err != nil {
			log.Fatal(err)
		}
		app.Hooks().OnStop(func(ctx context.Context) error {
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
