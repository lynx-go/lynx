package lynx

import (
	"encoding/json"
	"os"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type Options struct {
	ID         string `json:"id" flag:"id;;Server ID"`
	Name       string `json:"name" flag:"name;;service name, eg: --name lynx_app"`
	Version    string `json:"version"`
	SetFlags   func(f *pflag.FlagSet)
	LoadConfig func(c *viper.Viper) error
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

func WithSetFlags(f func(f *pflag.FlagSet)) Option {
	return func(o *Options) {
		o.SetFlags = f
	}
}

func WithLoadConfig(f func(c *viper.Viper) error) Option {
	return func(o *Options) {
		o.LoadConfig = f
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
