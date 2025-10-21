package main

import (
	"context"
	"encoding/json"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/log/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opts := lynx.NewOptions(
		lynx.WithSetFlags(func(f *pflag.FlagSet) {
			f.StringP("config", "c", "./configs", "config file path")
			f.String("addr", "", "http listen address")
			f.StringP("log_level", "l", "debug", "log level")
		}),
		lynx.WithBindConfig(func(f *pflag.FlagSet, v *viper.Viper) error {
			if c, _ := f.GetString("config"); c != "" {
				v.SetConfigFile(c)
			}
			v.SetEnvPrefix("LYNX_")
			v.AutomaticEnv()

			if err := v.BindEnv("addr", "LYNX_ADDR"); err != nil {
				return err
			}
			return nil
		}),
	)

	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.NewLogger(app))

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
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
			name := lynx.NameFromContext(app.Context())
			id := lynx.IDFromContext(app.Context())
			out, _ := json.Marshal(map[string]any{
				"hello": "world",
				"from":  name,
				"id":    id,
			})
			_, _ = rw.Write(out)
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
	app.Run()
}
