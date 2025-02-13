package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

type Option struct {
	Addr   string `json:"addr"`
	Config string `json:"config"`
}

type Config struct {
	Addr string `json:"addr"`
}

func main() {

	cli := lynx.NewCLI[Option](
		lynx.New(
			lynx.WithName[Option]("lynx-demo"),
			lynx.WithVersion[Option]("0.1.0"),
			lynx.WithSetup[Option](func(ctx context.Context, hooks *lynx.Hooks, o Option) (lynx.Runnable, error) {
				cfg := o.Config
				log.InfoContext(ctx, "config path", "path", cfg)
				hooks.Hook(&serviceServer{})
				hooks.Hook(&commandServer{})
				hooks.OnStart(func(ctx context.Context) error {
					log.InfoContext(ctx, "onstart")
					return nil
				})
				return lynx.RunForever(), nil
			}),
		),
		lynx.New[Option](
			lynx.WithName[Option]("hello"),
			lynx.WithVersion[Option]("0.1.0"),
			lynx.WithSetup[Option](func(ctx context.Context, hooks *lynx.Hooks, o Option) (lynx.Runnable, error) {
				log.InfoContext(ctx, "config path", "path", o.Config)
				hooks.Hook(&commandServer{})
				return func(ctx context.Context) error {
					log.InfoContext(ctx, "help")
					return nil
				}, nil
			}),
		),
	)

	cli.Run()
}

type commandServer struct {
}

func (s *commandServer) Start(ctx context.Context) error {
	log.InfoContext(ctx, "command-server start")
	return nil
}

func (s *commandServer) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "command-server stop")
	return nil
}

func (s *commandServer) Name() string {
	return "command-server"
}

var _ lynx.Hook = new(commandServer)

type serviceServer struct {
}

func (s *serviceServer) Start(ctx context.Context) error {
	log.InfoContext(ctx, "service-server start")
	return nil
}

func (s *serviceServer) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "service-server stop")
	return nil
}

func (s *serviceServer) Name() string {
	return "service-server"
}

var _ lynx.Hook = new(serviceServer)

type commonServer struct {
}

func (c *commonServer) Name() string {
	return "common-server"
}

func (c *commonServer) Start(ctx context.Context) error {
	log.InfoContext(ctx, "common-server start")
	return nil
}

func (c *commonServer) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "common-server stop")
	return nil
}

var _ lynx.Hook = new(commonServer)
