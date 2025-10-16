package lynx

import (
	"log"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

type Configurer interface {
	ConfigureGetter
	ConfigureUnmarshaler
	Sub(key string) Configurer
	Merge(data map[string]interface{})
}

type ConfigureGetter interface {
	Get(key string) any
	GetInt(key string) int
	GetBool(key string) bool
	GetString(key string) string
}

type ConfigureUnmarshaler interface {
	Unmarshal(v any) error
}

type viperConfig struct {
	v *viper.Viper
}

func (vc *viperConfig) Merge(data map[string]interface{}) {
	for k, v := range data {
		vc.v.Set(k, v)
	}
}

func (vc *viperConfig) Get(key string) any {
	return vc.v.Get(key)
}

func (vc *viperConfig) GetInt(key string) int {
	return vc.v.GetInt(key)
}

func (vc *viperConfig) GetBool(key string) bool {
	return vc.v.GetBool(key)
}

func (vc *viperConfig) GetString(key string) string {
	return vc.v.GetString(key)
}

func (vc *viperConfig) Unmarshal(v any) error {
	return vc.v.Unmarshal(v, func(config *mapstructure.DecoderConfig) {
		config.TagName = "json"
	})
}

func (vc *viperConfig) Sub(key string) Configurer {
	sub := vc.v.Sub(key)
	return &viperConfig{
		v: sub,
	}
}

var _ Configurer = new(viperConfig)

func newConfigFromFile(file string) Configurer {
	v := viper.New()
	v.SetConfigFile(file)

	if err := v.ReadInConfig(); err != nil {
		log.Fatal(err)
	}
	return &viperConfig{
		v: v,
	}
}

func newConfigFromDir(paths []string, fileType string) Configurer {
	v := viper.New()
	v.SetConfigType(fileType)

	for _, path := range paths {
		v.AddConfigPath(path)
	}

	if err := v.ReadInConfig(); err != nil {
		log.Fatal(err)
	}
	return &viperConfig{
		v: v,
	}
}
