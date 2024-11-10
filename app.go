package lynx

import (
	"context"
	"github.com/lynx-go/lynx/lifecycle"
)

type lifecycleWrapper struct {
	*lifecycle.Lifecycle
}

type Option struct {
}

func New() *App {
	return &App{}
}

type App struct {
	lifecycle *lifecycleWrapper
}

func (app *App) Start(ctx context.Context) (err error) {

	return
}

func (app *App) Stop(ctx context.Context) (err error) {
	return
}
