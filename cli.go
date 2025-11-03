package lynx

import (
	"context"
)

type SetupFunc func(ctx context.Context, app Lynx) error

type CLI struct {
	setup SetupFunc
	lynx  Lynx
}

func New(o *Options, setup SetupFunc) *CLI {
	app := newLynx(o)
	return &CLI{
		setup: setup,
		lynx:  app,
	}
}

func (app *CLI) Run() {
	if err := app.RunE(); err != nil {
		panic(err)
	}
}

func (app *CLI) RunE() error {
	if err := app.setup(app.lynx.Context(), app.lynx); err != nil {
		return err
	}
	return app.lynx.Run()
}
