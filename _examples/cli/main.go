package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/hook"
	"github.com/lynx-go/lynx/run"
	"github.com/lynx-go/x/log"
	"github.com/spf13/viper"
	"log/slog"
	"net/http"
	"sync/atomic"
)

type Option struct {
	Config string `json:"config"`
}

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	app := lynx.New[Option](lynx.WithName[Option]("system-test"), lynx.WithVersion[Option]("1"),
		lynx.WithSetup[Option](func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (run.RunFunc, error) {
			logger := log.FromContext(ctx)
			logger.Info("starting")
			viper.SetConfigFile(o.Config)
			if err := viper.ReadInConfig(); err != nil {
				return nil, err
			}
			c := &Config{}
			if err := viper.Unmarshal(&c); err != nil {
				return nil, err
			}

			server := newHttpServer(c.Addr)
			hooks.Register(server)
			hooks.OnStart(func(ctx context.Context) error {
				log.InfoContext(ctx, "onStart called")
				return nil
			})

			hooks.OnStop(func(ctx context.Context) error {
				slog.Info("onStop called")
				return nil
			})

			return func(ctx context.Context) error {
				log.InfoContext(ctx, "hello world")
				return nil
			}, nil
		}))
	o := Option{
		Config: "./_examples/system/config.yaml",
	}
	ctx := context.TODO()
	app.Run(ctx, o, []string{})
}

func newHttpServer(addr string) *httpServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("api called")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	return &httpServer{
		Server: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

type httpServer struct {
	*http.Server
	started atomic.Bool
}

func (h *httpServer) Status() (hook.Status, error) {
	if h.started.Load() {
		return hook.StatusStarted, nil
	}
	return hook.StatusUnstart, nil
}

func (h *httpServer) Name() string {
	return "http-server"
}

func (h *httpServer) Start(ctx context.Context) error {
	h.started.Store(true)
	log.InfoContext(ctx, fmt.Sprintf("%s starting", h.Name()))
	if err := h.ListenAndServe(); err != nil {
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	return nil
}

func (h *httpServer) Stop(ctx context.Context) error {
	log.InfoContext(ctx, fmt.Sprintf("%s stopping", h.Name()))
	return h.Server.Shutdown(ctx)
}

var _ hook.Hook = new(httpServer)
