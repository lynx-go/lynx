package lynx

import (
	"context"
	"fmt"
	"os"
)

type SetupFunc func(ctx context.Context, app Lynx) error

type CLI struct {
	setup SetupFunc
	lynx  Lynx
}

func New(o *Options, setup SetupFunc) *CLI {
	app, err := newLynx(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return &CLI{
		setup: setup,
		lynx:  app,
	}
}

func (app *CLI) Run() {
	if err := app.RunE(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func (app *CLI) RunE() error {
	if err := app.setup(app.lynx.Context(), app.lynx); err != nil {
		return err
	}
	return app.lynx.Run()
}
