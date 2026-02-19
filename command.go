package lynx

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/lynx-go/x/log"
	"github.com/pkg/errors"
)

type CommandFunc func(ctx context.Context) error

// CommandOptions configures the command component behavior.
type CommandOptions struct {
	MaxTries       uint
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// CommandOption is a function that configures CommandOptions.
type CommandOption func(*CommandOptions)

// WithMaxTries sets the maximum number of retry attempts.
func WithMaxTries(n uint) CommandOption {
	return func(o *CommandOptions) { o.MaxTries = n }
}

// WithBackoff sets the initial and maximum backoff durations.
func WithBackoff(initial, max time.Duration) CommandOption {
	return func(o *CommandOptions) {
		o.InitialBackoff = initial
		o.MaxBackoff = max
	}
}

// NewCommand creates a new command component with the given function and options.
func NewCommand(fn CommandFunc, opts ...CommandOption) Component {
	options := &CommandOptions{
		MaxTries:       10,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
	}
	for _, opt := range opts {
		opt(options)
	}
	return &command{fn: fn, options: options}
}

type command struct {
	fn      CommandFunc
	lynx    Lynx
	options *CommandOptions
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
	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.InitialInterval = cmd.options.InitialBackoff
	expBackoff.MaxInterval = cmd.options.MaxBackoff
	if _, err := backoff.Retry[any](ctx, func() (any, error) {
		for _, checker := range checkers {
			if err := checker.CheckHealth(); err != nil {
				log.WarnContext(ctx, "waiting for dependent component ready", "error", err)
				return nil, err
			}
		}
		return nil, nil
	}, backoff.WithMaxTries(cmd.options.MaxTries), backoff.WithBackOff(expBackoff)); err != nil {
		return errors.Wrap(err, "failed to start components")
	}
	return cmd.fn(ctx)
}

func (cmd *command) Stop(ctx context.Context) {
	cmd.lynx.Close()
}
