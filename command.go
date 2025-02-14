package lynx

import "context"

type Command[O any] struct {
	app         *App[O]
	desc        string
	usage       string
	aliases     []string
	subCommands []*Command[O]
}

func (cmd *Command[O]) RunE(ctx context.Context, o O, args []string) error {
	return cmd.app.RunE(ctx, o, args)
}

func (cmd *Command[O]) Name() string {
	return cmd.app.Name()
}

func (cmd *Command[O]) Desc() string {
	return cmd.desc
}

func (cmd *Command[O]) Usage() string {
	if cmd.usage == "" {
		return cmd.Name()
	}
	return cmd.usage
}

func (cmd *Command[O]) Aliases() []string {
	return cmd.aliases
}

func (cmd *Command[O]) SubCommands() []*Command[O] {
	return cmd.subCommands
}

type CommandOption[O any] func(cmd *Command[O])

func WithDesc[O any](desc string) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.desc = desc
	}
}

func WithUsage[O any](usage string) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.usage = usage
	}
}

func WithAlias[O any](aliases ...string) CommandOption[O] {
	return func(cmd *Command[O]) {
		cmd.aliases = append(cmd.aliases, aliases...)
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
	return cmd
}
