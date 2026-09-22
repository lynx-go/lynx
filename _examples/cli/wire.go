//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
)

func wireApp(app lynx.App) (*App, func(), error) {
	panic(wire.Build(ProviderSet))
}
