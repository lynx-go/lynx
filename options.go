package lynx

import (
	"encoding/json"
	"os"
)

type Options struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	SetFlagsFunc   SetFlagsFunc   `json:"-"`
	BindConfigFunc BindConfigFunc `json:"-"`
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

func WithSetFlags(f SetFlagsFunc) Option {
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

func WithBindConfig(f BindConfigFunc) Option {
	return func(o *Options) {
		o.BindConfigFunc = f
	}
}

func NewOptions(opts ...Option) *Options {
	id, _ := os.Hostname()
	op := &Options{
		ID: id,
	}
	for _, o := range opts {
		o(op)
	}
	return op
}
