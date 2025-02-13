package lynx

import (
	"context"
	"github.com/lynx-go/x/log"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"log/slog"
	"net/http"
	"testing"
)

func TestNewSystem(t *testing.T) {
	type Option struct {
		Config string `json:"config"`
	}

	type config struct{}
	sys := NewSystem[Option]("system-test", "1", func(ctx context.Context, hooks *Hooks, o Option) (Runnable, error) {
		logger := log.FromContext(ctx)
		logger.Info("starting")
		viper.SetConfigFile(o.Config)
		if err := viper.ReadInConfig(); err != nil {
			return nil, err
		}
		c := &config{}
		if err := viper.Unmarshal(&c); err != nil {
			return nil, err
		}

		server := newHttpServer()
		hooks.Server(server)
		hooks.OnStart(func(ctx context.Context) error {
			slog.Info("onStart called")
			return nil
		})

		hooks.OnStop(func(ctx context.Context) error {
			slog.Info("onStop called")
			return nil
		})

		return RunForever(), nil
	})
	o := Option{}
	ctx := context.TODO()
	err := sys.RunE(ctx, o)
	require.NoError(t, err)
}

func newHttpServer() *httpServer {
	return &httpServer{
		Server: &http.Server{
			Addr:    ":8080",
			Handler: http.DefaultServeMux,
		},
	}
}

type httpServer struct {
	*http.Server
}

func (h *httpServer) Name() string {
	return "http-server"
}

func (h *httpServer) Serve(ctx context.Context) error {
	return h.ListenAndServe()
}

func (h *httpServer) Shutdown(ctx context.Context) error {
	return h.Server.Shutdown(ctx)
}

var _ Server = new(httpServer)
