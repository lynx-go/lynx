package main

import (
	"context"
	"encoding/json"
	"log/slog"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/telemetry"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
)

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	builder := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
		logger.Info("parsed config", "config", config)

		// OTel 组件须先注册：Init 同步创建 provider 并设为全局，
		// 后续 initMetrics 创建的 instrument 才会挂到真实 MeterProvider 上。
		app.Register(telemetry.New())
		if err := initMetrics(); err != nil {
			return err
		}

		app.OnStart(func(ctx context.Context) error {
			app.Logger().Info("on start")
			return nil
		})

		app.OnStop(func(ctx context.Context) error {
			app.Logger().Info("on stop")
			return nil
		})
		router := gohttp.NewServeMux()
		router.HandleFunc("/", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			start := time.Now()
			helloRequestsCounter.Add(r.Context(), 1)
			defer func() {
				helloRequestDuration.Record(r.Context(), time.Since(start).Seconds())
			}()
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

		// Note: /metrics is served on the main router for demo simplicity, so
		// every Prometheus scrape also flows through the otel instrumentation
		// and latencyMiddleware. In production, consider serving it on a
		// separate mux or listener to avoid self-referential spans/metrics.
		router.Handle("/metrics", promhttp.Handler())

		app.Register(http.NewServer(router,
			http.WithAddr(addr),
			http.WithHealthCheckers(app.HealthCheckers),
			http.WithLogger(app.Logger("logger", "http-requestlog")),
			http.WithMiddleware(latencyMiddleware),
		))

		app.OnStart(func(ctx context.Context) error {
			time.Sleep(1 * time.Second)
			return nil
		})

		return nil
	},
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.StringP("config", "c", "./config.yaml", "config file path")
			f.String("addr", "", "http listen address")
			f.StringP("log_level", "l", "debug", "log level")
		}),
		lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
			if cf, _ := f.GetString("config"); cf != "" {
				c.SetFile(cf)
			}
			c.SetEnvPrefix("LYNX_")
			c.AutomaticEnv()

			if err := c.BindEnv("addr", "LYNX_ADDR"); err != nil {
				return err
			}
			return nil
		}),
	)
	builder.Run()
}

// latencyMiddleware is a demo lynx http.Middleware logging request latency.
func latencyMiddleware(next gohttp.Handler) gohttp.Handler {
	return gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Default().InfoContext(r.Context(), "request handled", "path", r.URL.Path, "latency", time.Since(start))
	})
}
