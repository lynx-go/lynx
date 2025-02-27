package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/hook"
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
		lynx.CMD[Option](
			lynx.New(
				lynx.WithName[Option]("lynx-demo"),
				lynx.WithVersion[Option]("0.1.0"),
				lynx.WithSetup[Option](func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (lynx.RunFunc, error) {
					cfg := o.Config
					log.InfoContext(ctx, "config path", "path", cfg)
					hooks.Register(&serviceServer{})
					hooks.Register(&commandServer{})
					hooks.OnStart(func(ctx context.Context) error {
						log.InfoContext(ctx, "onstart")
						return nil
					})
					return lynx.RunWaitSignal(), nil
				}),
			),
			lynx.WithSubCMD[Option](
				lynx.CMD[Option](
					lynx.New[Option](
						lynx.WithName[Option]("hello"),
						lynx.WithVersion[Option]("0.1.0"),
						lynx.WithSetup[Option](func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (lynx.RunFunc, error) {
							log.InfoContext(ctx, "config path", "path", o.Config)
							hooks.Register(&commandServer{})
							return func(ctx context.Context) error {
								log.InfoContext(ctx, "hello", "args", args, "options", o)
								return nil
							}, nil
						}),
					),
					lynx.WithDesc[Option]("print hello world"),
					lynx.WithSubCMD[Option](
						lynx.CMD[Option](
							lynx.New[Option](
								lynx.WithName[Option]("world"),
								lynx.WithVersion[Option]("0.1.0"),
								lynx.WithSetup[Option](func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (lynx.RunFunc, error) {
									log.InfoContext(ctx, "config path", "path", o.Config)
									hooks.Register(&commandServer{})
									return func(ctx context.Context) error {
										log.InfoContext(ctx, "hello world")
										return nil
									}, nil
								}),
							),
						),
					),
				),
			),
		),
	)

	cli.Run()
}

type commandServer struct {
}

func (s *commandServer) Status() (hook.Status, error) {
	return hook.StatusStarted, nil
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

var _ hook.Hook = new(commandServer)

type serviceServer struct {
}

func (s *serviceServer) Status() (hook.Status, error) {
	return hook.StatusStarted, nil
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

var _ hook.Hook = new(serviceServer)
