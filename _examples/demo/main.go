package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/lifecycle"
	"log/slog"
)

type Option struct {
	Addr   string `json:"addr"`
	Config string `json:"config"`
}

func main() {

	app := lynx.New[Option](
		lynx.WithName[Option]("lynx-demo"),
		lynx.WithSetup[Option](func(hooks *lifecycle.Registry, o Option) {
			cfg := o.Config
			slog.Info("config path", "path", cfg)

			hooks.Service(&serviceServer{})
			hooks.Service(&commandServer{})
			hooks.OnStart(func(ctx context.Context) error {
				slog.Info("onstart")
				return nil
			})
		}),
		lynx.WithCommands[Option](lynx.NewCommand[Option]("hello", "hello", "hello", func(ctx context.Context, o Option) error {
			
		})),
	)
	app.Run()
}

type OnStart struct {
	*lifecycle.HookBase
}

func (o *OnStart) OnStart(ctx context.Context) error {
	slog.Info("OnStart")
	return nil
}

type helloCommand[O any] struct {
	servers []lifecycle.Service
}

func (h *helloCommand[O]) Example() string {
	return ""
}

func (h *helloCommand[O]) SubCommands() []lynx.Command[O] {
	return []lynx.Command[O]{}
}

func (h *helloCommand[O]) Hooks() []lifecycle.Hook {
	hooks := []lifecycle.Hook{}
	for _, s := range h.servers {
		hooks = append(hooks, lifecycle.ServiceToHook(s))
	}
	return hooks
}

func (h *helloCommand[O]) Use() string {
	return "hello"
}

func (h *helloCommand[O]) Desc() string {
	return "hello world"
}

func (h *helloCommand[O]) Run(ctx context.Context, o O) error {
	slog.Info("hello world")
	return nil
}

var _ lynx.Command[Option] = new(helloCommand[Option])

type commandServer struct {
	*lifecycle.HookBase
}

func (s *commandServer) Start(ctx context.Context) error {
	slog.Info("command-server start")
	return nil
}

func (s *commandServer) Stop(ctx context.Context) {
	slog.Info("command-server stop")
}

func (s *commandServer) Name() string {
	return "command-server"
}

var _ lifecycle.Service = new(commandServer)

type serviceServer struct {
	*lifecycle.HookBase
}

func (s *serviceServer) Start(ctx context.Context) error {
	slog.Info("service-server start")
	return nil
}

func (s *serviceServer) Stop(ctx context.Context) {
	slog.Info("service-server stop")
}

func (s *serviceServer) IgnoreCLI() bool {
	return true
}

func (s *serviceServer) Name() string {
	return "service-server"
}

var _ lifecycle.Service = new(serviceServer)

type commonServer struct {
	*lifecycle.HookBase
}

func (c *commonServer) Name() string {
	return "common-server"
}

func (c *commonServer) Start(ctx context.Context) error {
	slog.Info("common-server start")
	return nil
}

func (c *commonServer) Stop(ctx context.Context) {
	slog.Info("common-server stop")
}

var _ lifecycle.Service = new(commonServer)
