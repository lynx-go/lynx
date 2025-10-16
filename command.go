package lynx

import "context"

type CommandFunc func(ctx context.Context) error

func NewCommand(fn CommandFunc) Component {
	return &command{fn: fn}
}

type command struct {
	fn   CommandFunc
	lynx Lynx
}

func (cmd *command) CheckHealth() error {
	return nil
}

func (cmd *command) Name() string {
	return "command"
}

func (cmd *command) Init(app Lynx) error {
	cmd.lynx = app
	return nil
}

func (cmd *command) Start(ctx context.Context) error {
	return cmd.fn(ctx)
}

func (cmd *command) Stop(ctx context.Context) {
	cmd.lynx.Close()
}
