package lynx

import (
	"context"
	"github.com/lynx-go/lynx/lifecycle"
)

type Command[O any] interface {
	Use() string
	Desc() string
	Example() string
	Run(ctx context.Context, o O) error
	Hooks() []lifecycle.Hook
	SubCommands() []Command[O]
}

func NewCommand[O any](use string, desc string, example string, run func(ctx context.Context, o O) error) Command[O] {
	return &command[O]{
		use:     use,
		desc:    desc,
		example: example,
		run:     run,
	}
}

type command[O any] struct {
	use     string
	desc    string
	example string
	run     func(context.Context, O) error
	hooks   []lifecycle.Hook
}

func (c *command[O]) Desc() string {
	return c.desc
}

func (c *command[O]) Example() string {
	return c.example
}

func (c *command[O]) Run(ctx context.Context, o O) error {
	return c.run(ctx, o)
}

func (c *command[O]) Hooks() []lifecycle.Hook {
	return c.hooks
}

func (c *command[O]) SubCommands() []Command[O] {
	return []Command[O]{}
}

func (c *command[O]) Use() string {
	return c.use
}
