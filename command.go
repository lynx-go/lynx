package lynx

import (
	"context"

	"github.com/cenkalti/backoff/v5"
	"github.com/lynx-go/x/log"
	"github.com/pkg/errors"
)

type CommandFunc func(ctx context.Context) error

func NewCommand(fn CommandFunc) Component {
	return &command{fn: fn}
}

type command struct {
	fn   CommandFunc
	lynx Lynx
}

func (cmd *command) Name() string {
	return "command"
}

func (cmd *command) Init(app Lynx) error {
	cmd.lynx = app
	return nil
}

func (cmd *command) Start(ctx context.Context) error {
	checkers := cmd.lynx.HealthCheckFunc()()
	if _, err := backoff.Retry[any](ctx, func() (any, error) {
		for _, checker := range checkers {
			if err := checker.CheckHealth(); err != nil {
				log.WarnContext(ctx, "waiting for dependent component ready", "error", err)
				return nil, err
			}
		}
		return nil, nil
	}, backoff.WithMaxTries(10), backoff.WithBackOff(backoff.NewExponentialBackOff())); err != nil {
		return errors.Wrap(err, "failed to start components")
	}
	return cmd.fn(ctx)
}

func (cmd *command) Stop(ctx context.Context) {
	cmd.lynx.Close()
}
