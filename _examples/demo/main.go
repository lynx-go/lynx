package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/hook"
	"github.com/lynx-go/x/log"
	"net/http"
	"os"
)

type Option struct {
	Addr   string `json:"addr"`
	Config string `json:"config"`
}

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	serverSetup := func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (lynx.RunFunc, error) {
		cfg := o.Config
		log.InfoContext(ctx, "config path", "path", cfg)
		hooks.Register(&serviceServer{addr: o.Addr})
		hooks.Register(&commandServer{})
		hooks.OnStart(func(ctx context.Context) error {
			log.InfoContext(ctx, "onstart")
			return nil
		})
		return lynx.WaitSignals(), nil
	}

	helloSetup := func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (lynx.RunFunc, error) {
		log.InfoContext(ctx, "config path", "path", o.Config)
		hooks.Register(&commandServer{})
		return func(ctx context.Context) error {
			log.InfoContext(ctx, "hello", "args", args, "options", o)
			return nil
		}, nil
	}

	worldSetup := func(ctx context.Context, hooks *hook.Hooks, o Option, args []string) (lynx.RunFunc, error) {
		log.InfoContext(ctx, "config path", "path", o.Config)
		hooks.Register(&commandServer{})
		return func(ctx context.Context) error {
			log.InfoContext(ctx, "hello world")
			return nil
		}, nil
	}
	id, _ := os.Hostname()

	cli := lynx.NewCLI[Option](
		lynx.CMD[Option](
			lynx.New(
				lynx.WithMeta[Option](&lynx.Meta{
					ID:      id,
					Name:    "lynx",
					Version: "0.0.1",
				}),
				lynx.WithSetup[Option](serverSetup),
			),
			lynx.WithCMD[Option](
				lynx.CMD[Option](
					lynx.New[Option](
						lynx.WithMeta[Option](&lynx.Meta{
							ID:      id,
							Name:    "lynx",
							Version: "0.0.1",
						}),
						lynx.WithSetup[Option](helloSetup),
					),
					lynx.WithUsage[Option]("hello"),
					lynx.WithDesc[Option]("print hello world"),
					lynx.WithCMD[Option](
						lynx.CMD[Option](
							lynx.New[Option](
								lynx.WithMeta[Option](&lynx.Meta{
									ID:      id,
									Name:    "lynx",
									Version: "0.0.1",
								}),
								lynx.WithSetup[Option](worldSetup),
							),
							lynx.WithUsage[Option]("world"),
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
	addr string
}

func (s *serviceServer) Status() (hook.Status, error) {
	return hook.StatusStarted, nil
}

func (s *serviceServer) Start(ctx context.Context) error {
	log.InfoContext(ctx, "service-server start")
	return http.ListenAndServe(s.addr, http.NewServeMux())
}

func (s *serviceServer) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "service-server stop")
	return nil
}

func (s *serviceServer) Name() string {
	return "service-server"
}

var _ hook.Hook = new(serviceServer)
