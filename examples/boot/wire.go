//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.
package main

import (
	"log/slog"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/bootstrap"
)

func wireBootstrap(ft lynx.Lynx, o *lynx.Options, c lynx.Configurer, slogger *slog.Logger) (*bootstrap.Bootstrap, func(), error) {
	panic(wire.Build(ProviderSet))
}
