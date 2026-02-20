package lynx

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/go-viper/mapstructure/v2"
)

// Default values for Options.
const (
	DefaultName         = "lynx-app"
	DefaultCloseTimeout = 5 * time.Second
	MinCloseTimeout     = 1 * time.Second
	MaxCloseTimeout     = 5 * time.Minute
)

// Validation errors for Options.
var (
	ErrNameTooLong          = errors.New("name must be at most 63 characters")
	ErrCloseTimeoutTooSmall = errors.New("close timeout must be at least 1 second")
	ErrCloseTimeoutTooLarge = errors.New("close timeout must be at most 5 minutes")
)

type Options struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	SetFlagsFunc   SetFlagsFunc   `json:"-"`
	BindConfigFunc BindConfigFunc `json:"-"`
	ExitSignals    []os.Signal    `json:"-"`
	CloseTimeout   time.Duration  `json:"close_timeout"`
}

func (o *Options) String() string {
	bs, _ := json.Marshal(o)
	return string(bs)

}

// Validate checks if the Options values are valid.
func (o *Options) Validate() error {
	if len(o.Name) > 63 {
		return ErrNameTooLong
	}
	if o.CloseTimeout > 0 {
		if o.CloseTimeout < MinCloseTimeout {
			return ErrCloseTimeoutTooSmall
		}
		if o.CloseTimeout > MaxCloseTimeout {
			return ErrCloseTimeoutTooLarge
		}
	}
	return nil
}

// EnsureDefaults sets default values for unset fields and validates the options.
func (o *Options) EnsureDefaults() {
	if o.ID == "" {
		o.ID, _ = os.Hostname()
	}

	if o.Name == "" {
		o.Name = DefaultName
	}

	if o.CloseTimeout == 0 {
		o.CloseTimeout = DefaultCloseTimeout
	}

	if len(o.ExitSignals) == 0 {
		o.ExitSignals = []os.Signal{
			syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL,
		}
	}
}

type Option func(*Options)

func WithID(id string) Option {
	return func(o *Options) {
		o.ID = id
	}
}

func WithName(name string) Option {
	return func(o *Options) {
		o.Name = name
	}
}

func WithVersion(v string) Option {
	return func(o *Options) {
		o.Version = v
	}
}

func WithSetFlagsFunc(f SetFlagsFunc) Option {
	return func(o *Options) {
		o.SetFlagsFunc = f
	}
}

func WithUseDefaultConfigFlagsFunc() Option {
	return func(o *Options) {
		o.BindConfigFunc = DefaultBindConfigFunc
		o.SetFlagsFunc = DefaultSetFlagsFunc
	}
}

func WithBindConfigFunc(f BindConfigFunc) Option {
	return func(o *Options) {
		o.BindConfigFunc = f
	}
}

func WithExitSignals(signals ...os.Signal) Option {
	return func(o *Options) {
		o.ExitSignals = signals
	}
}

func WithCloseTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.CloseTimeout = timeout
	}
}

func NewOptions(opts ...Option) *Options {
	id, _ := os.Hostname()
	op := &Options{
		ID:           id,
		ExitSignals:  []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL},
		CloseTimeout: DefaultCloseTimeout,
	}
	for _, o := range opts {
		o(op)
	}
	return op
}

func TagNameJSON(config *mapstructure.DecoderConfig) {
	config.TagName = "json"
}

func TagNameYAML(config *mapstructure.DecoderConfig) {
	config.TagName = "yaml"
}
