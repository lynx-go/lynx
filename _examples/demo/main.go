package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/config"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"log/slog"
)

type Config struct{}

func main() {
	v, _ := config.Configure("lynx-demo", func(v *viper.Viper, f *pflag.FlagSet) {
	})
	v.WatchConfig()
	c := &Config{}
	if err := v.Unmarshal(c); err != nil {
		panic(err)
	}
	app := lynx.New(
		lynx.WithName("lynx-demo"),
		lynx.WithServer(&commonServer{c: c}, &serviceServer{}),
		lynx.WithOnStart(func(ctx context.Context) error {
			slog.Info("start hook")
			return nil
		}),
		lynx.WithOnStop(func(ctx context.Context) error {
			slog.Info("stop hook")
			return nil
		}),
		lynx.WithCommands(&helloCommand{}),
	)
	app.Run()
}

type helloCommand struct {
}

func (h *helloCommand) Servers() []lynx.Server {
	return []lynx.Server{
		&commandServer{},
	}
}

func (h *helloCommand) Name() string {
	return "hello"
}

func (h *helloCommand) Description() string {
	return "hello world"
}

func (h *helloCommand) Command(ctx context.Context, args []string) error {
	slog.Info("hello world")
	return nil
}

var _ lynx.Command = new(helloCommand)

type commandServer struct {
}

func (cs *commandServer) Name() string {
	return "command-server"
}

func (cs *commandServer) Start(ctx context.Context) error {
	slog.Info("start command-server")
	return nil
}

func (cs *commandServer) Stop(ctx context.Context) error {
	slog.Info("stop command-server")
	return nil
}

var _ lynx.Server = new(commandServer)

type serviceServer struct {
}

func (ss *serviceServer) NotForCLI() bool {
	return true
}

func (ss *serviceServer) Name() string {
	return "service-server"
}

func (ss *serviceServer) Start(ctx context.Context) error {
	slog.Info("start service server")
	return nil
}

func (ss *serviceServer) Stop(ctx context.Context) error {
	slog.Info("stop service server")
	return nil
}

var _ lynx.Server = new(serviceServer)
var _ lynx.NotForCLI = new(serviceServer)

type commonServer struct {
	c *Config
}

func (cs *commonServer) Name() string {
	return "common"
}

func (cs *commonServer) Start(ctx context.Context) error {
	slog.Info("start common service")
	return nil
}

func (cs *commonServer) Stop(ctx context.Context) error {
	slog.Info("stop common service")
	return nil
}

var _ lynx.Server = new(commonServer)
