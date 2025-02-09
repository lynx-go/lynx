package config

import (
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"os"
	"strings"
)

func Configure(name string, binder func(v *viper.Viper, f *pflag.FlagSet)) (*viper.Viper, *pflag.FlagSet) {
	v, f := viper.New(), pflag.NewFlagSet(name, pflag.ExitOnError)
	configure(v, f)
	binder(v, f)
	f.String("config", "", "Configuration file")

	_ = f.Parse(os.Args[1:])
	if c, _ := f.GetString("config"); c != "" {
		v.SetConfigFile(c)
	}

	return v, f
}

// configure configures some defaults in the Viper instance.
func configure(v *viper.Viper, f *pflag.FlagSet) {
	// Viper settings
	v.AddConfigPath(".")
	v.AddConfigPath("$CONFIG_DIR/")

	// Environment variable settings
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AllowEmptyEnv(true)
	v.AutomaticEnv()
}
