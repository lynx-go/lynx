package main

import (
	"context"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/lynx-go/lynx/server/http"
)

//go:generate wire

var ProviderSet = wire.NewSet(
	boot.New,
	NewHttpServer,
	NewConfig,
	NewServices,
	NewServiceFactories,
	NewPreStarts,
	NewDrains,
	NewPreStops,
	NewPostStops,
)

func NewConfig(app lynx.App) (*AppConfig, error) {
	c := new(AppConfig)
	if err := app.Config().Unmarshal(c); err != nil {
		return nil, err
	}
	return c, nil
}

func NewServices(hs *http.Server) []lynx.Service {
	return []lynx.Service{hs}
}

func NewServiceFactories() []lynx.ServiceFactory {
	return []lynx.ServiceFactory{}
}

func NewPreStarts(app lynx.App) boot.PreStartHooks {
	return boot.PreStartHooks{
		func(ctx context.Context) error {
			app.Logger().Info("starting")
			return nil
		},
	}
}

func NewDrains() boot.DrainHooks {
	return boot.DrainHooks{}
}

func NewPreStops(app lynx.App) boot.PreStopHooks {
	return boot.PreStopHooks{
		func(ctx context.Context) error {
			app.Logger().Info("stopping")
			return nil
		},
	}
}

func NewPostStops() boot.PostStopHooks {
	return boot.PostStopHooks{}
}
