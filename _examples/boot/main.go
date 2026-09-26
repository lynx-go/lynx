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
		boot.Apply(app)
		return nil
	},
		lynx.WithLoggerProvider(zap.NewLogger),
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.String("addr", ":8080", "http listen address")
			// 默认值留空：非空默认会经下方翻译以 Set 覆盖配置文件的
			// logging.level（内置 --log-level 的同一约束）。
			f.StringP("loglevel", "l", "", "log level override, e.g. debug")
			f.StringP("config", "c", "", "config file path")
		}),
		lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
			if cf, _ := f.GetString("config"); cf != "" {
				c.SetFile(cf)
			}
			// 自定义 flag 不在框架翻译范围内（框架只翻译内置 --log-level）：
			// 在此翻译进规范键 logging.level，模式与 lynx 的内置翻译一致
			//（lynx.initConfigure）。没有这一步，-l/--loglevel 就是死线——
			// viper 里躺着一个没人读取的 loglevel 键。
			if lv, _ := f.GetString("loglevel"); lv != "" {
				c.Set("logging.level", lv)
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
