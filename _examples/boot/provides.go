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
	NewComponents,
	NewComponentBuilders,
	NewOnStarts,
	NewOnStops,
)

func NewConfig(app lynx.Lynx) (*AppConfig, error) {
	c := new(AppConfig)
	if err := app.Config().Unmarshal(c); err != nil {
		return nil, err
	}
	return c, nil
}

func NewComponents(hs *http.Server) []lynx.Component {
	return []lynx.Component{hs}
}

func NewComponentBuilders() []lynx.ComponentBuilder {
	return []lynx.ComponentBuilder{}
}

func NewOnStarts(app lynx.Lynx) lynx.OnStartHooks {
	return lynx.OnStartHooks{
		func(ctx context.Context) error {
			app.Logger().Info("starting")
			return nil
		},
	}
}

func NewOnStops(app lynx.Lynx) lynx.OnStopHooks {
	return lynx.OnStopHooks{
		func(ctx context.Context) error {
			app.Logger().Info("stopping")
			return nil
		},
	}
}
