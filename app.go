package lynx

import (
	"context"
	"github.com/lynx-go/lynx/lifecycle"
)

type lifecycleWrapper struct {
	*lifecycle.Lifecycle
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
