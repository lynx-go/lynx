package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/hook"
	"log/slog"
)

type Option struct {
	Addr   string `json:"addr"`
	Config string `json:"config"`
}

func main() {

	app := lynx.New[Option](
		lynx.WithName[Option]("lynx-demo"),
		lynx.WithBootstrap[Option](func(hooks *hook.Registry, o Option) {
			cfg := o.Config
			slog.Info("config path", "path", cfg)
			hooks.Register(&OnStart{})
			hooks.Register(&serviceServer{})
			hooks.Register(&commandServer{})
		}),
		lynx.WithCommands[Option](&helloCommand{
			servers: []lynx.Server{&commandServer{}},
		}),
	)
	app.Run()
}

type OnStart struct {
	*hook.HookBase
}

func (o *OnStart) OnStart(ctx context.Context) error {
	slog.Info("OnStart")
	return nil
}

type helloCommand struct {
	servers []lynx.Server
}

func (h *helloCommand) Hooks() []hook.Hook {
	hooks := []hook.Hook{}
	for _, s := range h.servers {
		hooks = append(hooks, s)
	}
	return hooks
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
	*hook.HookBase
}

func (s *commandServer) OnStart(ctx context.Context) error {
	slog.Info("command-server start")
	return nil
}

func (s *commandServer) OnStop(ctx context.Context) {
	slog.Info("command-server stop")
}

func (s *commandServer) Name() string {
	return "command-server"
}

var _ lynx.Server = new(commandServer)

type serviceServer struct {
	*hook.HookBase
}

func (s *serviceServer) OnStart(ctx context.Context) error {
	slog.Info("service-server start")
	return nil
}

func (s *serviceServer) OnStop(ctx context.Context) {
	slog.Info("service-server stop")
}

func (s *serviceServer) IgnoreForCLI() bool {
	return true
}

func (s *serviceServer) Name() string {
	return "service-server"
}

var _ lynx.Server = new(serviceServer)

type commonServer struct {
	*hook.HookBase
}

func (c *commonServer) Name() string {
	return "common-server"
}

func (c *commonServer) OnStart(ctx context.Context) error {
	slog.Info("common-server start")
	return nil
}

func (c *commonServer) OnStop(ctx context.Context) {
	slog.Info("common-server stop")
}

var _ lynx.Server = new(commonServer)
