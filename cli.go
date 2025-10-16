package lynx

import (
	"context"
	"os"
)

type SetupFunc func(ctx context.Context, app Lynx) error

type App struct {
	setup SetupFunc
	lynx  Lynx
}

func New(o Options, setup SetupFunc) *App {
	if o.ID == "" {
		o.ID, _ = os.Hostname()
	}
	app := newLynx(o)
	return &App{
		setup: setup,
		lynx:  app,
	}
}

func (app *App) Run() {
	if err := app.RunE(); err != nil {
		panic(err)
	}
}

func (app *App) RunE() error {
	if err := app.setup(app.lynx.Context(), app.lynx); err != nil {
		return err
	}
	return app.lynx.Run()
}
