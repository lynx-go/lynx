package lynx

import (
	"encoding/json"
	"github.com/AdamSLevy/flagbind"
	"github.com/spf13/pflag"
	"log"
	"os"
	"strings"
	"time"
)

type Options struct {
	ID              string        `json:"id" flag:"id;;Server ID"`
	ConfigDir       string        `json:"config_dir" flag:"config-dir;;config path, eg:--config-dir ./configs"`
	ConfigType      string        `json:"config_type" flag:"config-type;yaml;config file type, eg:--config-type yaml"`
	Config          string        `json:"config" flag:"config;config file, eg: --config ./configs/config.yaml"`
	LogLevel        string        `json:"log_level" flag:"log-level;debug;default log level, eg:--log-level debug"`
	Properties      string        `json:"properties" flag:"properties;;extern args, eg: --properties a=1,b=2"`
	Version         string        `json:"version"`
	Name            string        `json:"name"`
	ShutdownTimeout time.Duration `json:"shutdown_timeout"`
}

func (o *Options) String() string {
	bs, _ := json.Marshal(o)
	return string(bs)

}
func (o *Options) EnsureDefaults() {
	if o.ID == "" {
		o.ID, _ = os.Hostname()
	}
	o.ShutdownTimeout = 5 * time.Second
}

func (o *Options) PropertiesAsMap() map[string]any {
	properties := map[string]any{}
	args := strings.Split(o.Properties, ",")
	for _, s := range args {
		kv := strings.Split(s, "=")
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		properties[key] = value
	}
	return properties
}

func BindOptions() Options {
	fs := pflag.NewFlagSet("", pflag.ExitOnError)
	option := Options{}
	if err := flagbind.Bind(fs, &option); err != nil {
		log.Fatalln(err)
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
	return option
}
