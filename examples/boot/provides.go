package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/bootstrap"
	"github.com/lynx-go/lynx/server/http"
)

//go:generate wire

var ProviderSet = wire.NewSet(
	bootstrap.New,
	NewHttpServer,
	NewConfig,
	NewComponents,
	NewComponentFactories,
	NewOnStarts,
	NewOnStops,
)

func NewConfig(lx lynx.Lynx) (*AppConfig, error) {
	c := new(AppConfig)
	if err := lx.Config().Unmarshal(c); err != nil {
		return nil, err
	}
	return c, nil
}

func NewComponents(hs *http.Server) []lynx.Component {
	return []lynx.Component{hs}
}

func NewComponentFactories() []lynx.ComponentFactory {
	return []lynx.ComponentFactory{}
}

func NewOnStarts() lynx.OnStartHooks {
	return lynx.OnStartHooks{}
}

func NewOnStops() lynx.OnStopHooks {
	return lynx.OnStopHooks{}
}
