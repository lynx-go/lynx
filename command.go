package lynx

import "context"

type Command[O any] struct {
	app         *App[O]
	name        string
	usage       string
	alias       string
	subCommands []*Command[O]
}

func (cmd *Command[O]) RunE(ctx context.Context, o O) error {
	return cmd.app.RunE(ctx, o)
}

func (cmd *Command[O]) Name() string {
	if cmd.name == "" {
		return cmd.app.Name()
	}
	return cmd.name
}

func (cmd *Command[O]) Usage() string {
	if cmd.usage == "" {
		return cmd.Name()
	}
	return cmd.usage
}

func (cmd *Command[O]) Aliases() []string {
	return []string{cmd.alias}
}

func (cmd *Command[O]) SubCommands() []*Command[O] {
	return cmd.subCommands
}

type CommandOption[O any] func(cmd *Command[O])

func WithCommandName[O any](name string) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.name = name
	}
}

func WithCommandUsage[O any](usage string) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.usage = usage
	}
}

func WithCommandAlias[O any](alias string) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.alias = alias
	}
}

func WithSubCMD[O any](subCommands ...*Command[O]) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.subCommands = append(cmd.subCommands, subCommands...)
	}
}

func CMD[O any](app *App[O], opts ...CommandOption[O]) *Command[O] {
	cmd := &Command[O]{app: app}
	for _, opt := range opts {
		opt(cmd)
	}
	if cmd.name == "" {
		cmd.name = app.Name()
	}
	return cmd
}
