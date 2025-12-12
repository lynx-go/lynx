package lynx

import (
	"encoding/json"
	"os"
	"syscall"
	"time"

	"github.com/go-viper/mapstructure/v2"
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
func (o *Options) EnsureDefaults() {
	if o.ID == "" {
		o.ID, _ = os.Hostname()
	}

	if o.Name == "" {
		o.Name = "lynx-app"
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
		ID: id,
		ExitSignals: []os.Signal{
			syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL,
		},
		CloseTimeout: time.Second * 5,
	}
	for _, o := range opts {
		o(op)
	}
	return op
}

func TagNameJSON(config *mapstructure.DecoderConfig) {
	config.TagName = "json"
}
