package main

import (
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
		// Wire 的 cleanup（释放 DB/Redis 连接池等 DI 底层资源）挂
		// OnPostStop：所有服务 Stop、总线关停之后才执行，自带
		// CleanupTimeout 预算。不要放 OnPreStop——它先于服务 Stop 执行，
		// 排水/关停期间在途请求还要用这些资源（v1.10.0 前本示例挂在
		// OnStop 上，正是这个坑）。
		app.OnPostStop(cleanup)
		boot.Bind(app)
		return nil
	},
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
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
