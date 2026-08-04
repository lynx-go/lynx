package lynx

import (
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// DecoderConfigOption 定制 mapstructure 解码行为，与 viper.DecoderConfigOption
// 底层类型相同，可通过显式转换互相转换。
type DecoderConfigOption func(*mapstructure.DecoderConfig)

// Config 是应用配置的访问接口，默认实现为适配 *viper.Viper 的适配器。
// 需要 Viper 完整能力（如自定义配置源）时，可在 BindConfigFunc 中持有
// *viper.Viper 引用，或通过 NewViperConfig 包装自己的实例。
type Config interface {
	// Get 返回 key 对应的值，未设置时返回 nil。
	Get(key string) any
	// GetString 返回 key 对应的字符串值。
	GetString(key string) string
	// GetBool 返回 key 对应的布尔值。
	GetBool(key string) bool
	// GetInt 返回 key 对应的整数值。
	GetInt(key string) int
	// GetStringMap 返回 key 对应的键值映射。
	GetStringMap(key string) map[string]any
	// GetStringSlice 返回 key 对应的字符串切片。
	GetStringSlice(key string) []string
	// IsSet 报告 key 是否已设置。
	IsSet(key string) bool
	// Set 设置 key 的值。
	Set(key string, value any)
	// Unmarshal 将配置解码到 rawVal 指向的结构体，可通过 DecoderConfigOption
	// 定制解码行为（如 lynx.TagNameJSON / lynx.TagNameYAML）。
	Unmarshal(rawVal any, opts ...DecoderConfigOption) error
}

// NewViperConfig 将 *viper.Viper 包装为 Config，是 Config 的默认实现。
func NewViperConfig(v *viper.Viper) Config {
	return &viperConfig{v: v}
}

// viperConfig 是 Config 的默认实现，适配 *viper.Viper。
type viperConfig struct {
	v *viper.Viper
}

func (c *viperConfig) Get(key string) any {
	return c.v.Get(key)
}

func (c *viperConfig) GetString(key string) string {
	return c.v.GetString(key)
}

func (c *viperConfig) GetBool(key string) bool {
	return c.v.GetBool(key)
}

func (c *viperConfig) GetInt(key string) int {
	return c.v.GetInt(key)
}

func (c *viperConfig) GetStringMap(key string) map[string]any {
	return c.v.GetStringMap(key)
}

func (c *viperConfig) GetStringSlice(key string) []string {
	return c.v.GetStringSlice(key)
}

func (c *viperConfig) IsSet(key string) bool {
	return c.v.IsSet(key)
}

func (c *viperConfig) Set(key string, value any) {
	c.v.Set(key, value)
}

func (c *viperConfig) Unmarshal(rawVal any, opts ...DecoderConfigOption) error {
	viperOpts := make([]viper.DecoderConfigOption, 0, len(opts))
	for _, opt := range opts {
		viperOpts = append(viperOpts, viper.DecoderConfigOption(opt))
	}
	return c.v.Unmarshal(rawVal, viperOpts...)
}
