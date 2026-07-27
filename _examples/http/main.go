package main

import (
	"context"
	"encoding/json"
	"log/slog"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	opts := lynx.NewOptions(
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.StringP("config", "c", "./configs", "config file path")
			f.String("addr", "", "http listen address")
			f.StringP("log_level", "l", "debug", "log level")
		}),
		lynx.WithBindConfigFunc(func(f *pflag.FlagSet, v *viper.Viper) error {
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

	cli := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.MustNewLogger(app))

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
		logger.Info("parsed config", "config", config)

		if err := app.Hooks(lynx.OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})); err != nil {
			return err
		}

		if err := app.Hooks(lynx.OnStop(func(ctx context.Context) error {
			app.Logger().Info("on stop")
			return nil
		})); err != nil {
			return err
		}
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

		shutdown, tp, mp, propagator, err := setupOTel()
		if err != nil {
			return err
		}
		// Provider lifecycle belongs to the caller: shut them down when the
		// app stops, not when this setup function returns.
		if err := app.Hooks(lynx.OnStop(func(ctx context.Context) error {
			return shutdown(ctx)
		})); err != nil {
			return err
		}

		router.Handle("/metrics", promhttp.Handler())

		if err := app.Hooks(lynx.Components(http.NewServer(router,
			http.WithAddr(addr),
			http.WithHealthCheck(app.HealthCheckFunc()),
			http.WithLogger(app.Logger("logger", "http-requestlog")),
			http.WithTracerProvider(tp),
			http.WithMeterProvider(mp),
			http.WithPropagator(propagator),
			http.WithMiddleware(latencyMiddleware),
		))); err != nil {
			return err
		}

		if err := app.Hooks(lynx.OnStart(func(ctx context.Context) error {
			time.Sleep(1 * time.Second)
			return nil
		})); err != nil {
			return err
		}

		return nil
	})
	cli.Run()
}

// latencyMiddleware is a demo lynx http.Middleware logging request latency.
func latencyMiddleware(next gohttp.Handler) gohttp.Handler {
	return gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Default().InfoContext(r.Context(), "request handled", "path", r.URL.Path, "latency", time.Since(start))
	})
}
