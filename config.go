package lynx

import (
	"strings"

	"github.com/spf13/viper"
)

// Config 是应用配置的通用读取接口，与具体配置库解耦。
// 默认实现适配 *viper.Viper（见 NewViperConfig）。
// path 使用点分路径（如 "logging.level"），具体语义由实现定义。
type Config interface {
	// Get 返回 path 对应的值，未设置时返回 nil。
	Get(path string) any
	// GetString 返回 path 对应的字符串值。
	GetString(path string) string
	// GetBool 返回 path 对应的布尔值。
	GetBool(path string) bool
	// GetInt 返回 path 对应的整数值。
	GetInt(path string) int
	// GetStringMap 返回 path 对应的键值映射。
	GetStringMap(path string) map[string]any
	// GetStringSlice 返回 path 对应的字符串切片。
	GetStringSlice(path string) []string
	// IsSet 报告 path 是否已设置。
	IsSet(path string) bool
	// Unmarshal 将配置解码到 out 指向的结构体。默认行为与 viper 一致，
	// 通过 opts 选择或定制解码行为（见 UnmarshalOption）。
	Unmarshal(out any, opts ...UnmarshalOption) error
	// UnmarshalKey 将 path 对应的配置子树解码到 out 指向的结构体；
	// opts 语义与 Unmarshal 相同，叶子键以 path 为前缀。
	UnmarshalKey(path string, out any, opts ...UnmarshalOption) error
}

// UnmarshalOptions 是 Unmarshal/UnmarshalKey 的解码设置。
// 字段均为解码器无关概念，任意 Config 实现都应能解释。
type UnmarshalOptions struct {
	// TagName 非空时仅按该 struct tag 匹配配置键（无 tag 的字段按
	// 小写字段名匹配）。空值时：默认路径仅按 mapstructure 匹配（viper
	// 语义）；EnvForAllKeys 路径按 mapstructure → json → 小写字段名回退。
	TagName string
	// EnvForAllKeys 开启结构体驱动的逐叶取值：以 out 的结构体叶子为键集
	// 逐键 Get，使仅在环境变量中设置的键（配置文件无此键）也参与解码——
	// viper Unmarshal 基于 AllSettings，对此类键不可见。无法枚举叶子的
	// 目标（非结构体、动态键的 map 字段）回落 viper 路径。
	EnvForAllKeys bool
}

// UnmarshalOption 定制 Unmarshal/UnmarshalKey 的解码行为。
type UnmarshalOption func(*UnmarshalOptions)

// WithTagName 指定匹配配置键所用的 struct tag。
func WithTagName(tag string) UnmarshalOption {
	return func(o *UnmarshalOptions) { o.TagName = tag }
}

// WithEnvForAllKeys 开启结构体驱动的逐叶取值，使仅在环境变量中设置的键
// 也参与解码。
func WithEnvForAllKeys() UnmarshalOption {
	return func(o *UnmarshalOptions) { o.EnvForAllKeys = true }
}

// ConfigSource 是配置源的绑定接口，在初始化绑定阶段（BindConfigFunc）
// 使用，在 Config 的基础上增加写入与配置源管理方法。
// 默认实现同样适配 *viper.Viper。
type ConfigSource interface {
	Config
	// Set 设置 path 的值。
	Set(path string, value any)
	// SetFile 设置配置文件路径。
	SetFile(path string)
	// AddSearchPath 添加配置文件搜索目录。
	AddSearchPath(dir string)
	// SetFileFormat 设置配置文件格式（如 yaml、json）。
	SetFileFormat(format string)
	// SetEnvPrefix 设置环境变量前缀。
	SetEnvPrefix(prefix string)
	// SetEnvKeyReplacer 设置环境变量键名替换规则，用于把点分路径键映射为
	// 环境变量名（如 "." → "_"），使任意键都能被环境变量覆盖。
	SetEnvKeyReplacer(repl *strings.Replacer)
	// AutomaticEnv 启用环境变量自动匹配。
	AutomaticEnv()
	// BindEnv 将 path 绑定到环境变量；env 为空时使用 path 的默认环境变量形式。
	BindEnv(path string, env ...string) error
}

// NewViperConfig 将 *viper.Viper 包装为 ConfigSource（同时也是 Config），
// 是框架的默认实现。其他配置库可自行实现 Config / ConfigSource 接入。
func NewViperConfig(v *viper.Viper) ConfigSource {
	return &viperConfig{v: v}
}

// viperConfig 是 ConfigSource 的默认实现，适配 *viper.Viper。
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

func (c *viperConfig) Unmarshal(out any, opts ...UnmarshalOption) error {
	o := applyUnmarshalOptions(opts)
	if o.EnvForAllKeys && structTypeOf(out) != nil {
		return unmarshalByStruct(c, "", out, o)
	}
	return c.v.Unmarshal(out, o.viperOpts()...)
}

func (c *viperConfig) UnmarshalKey(path string, out any, opts ...UnmarshalOption) error {
	o := applyUnmarshalOptions(opts)
	if o.EnvForAllKeys && structTypeOf(out) != nil {
		return unmarshalByStruct(c, path, out, o)
	}
	return c.v.UnmarshalKey(path, out, o.viperOpts()...)
}

func (c *viperConfig) Set(key string, value any) {
	c.v.Set(key, value)
}

func (c *viperConfig) SetFile(path string) {
	c.v.SetConfigFile(path)
}

func (c *viperConfig) AddSearchPath(dir string) {
	c.v.AddConfigPath(dir)
}

func (c *viperConfig) SetFileFormat(format string) {
	c.v.SetConfigType(format)
}

func (c *viperConfig) SetEnvPrefix(prefix string) {
	c.v.SetEnvPrefix(prefix)
}

func (c *viperConfig) SetEnvKeyReplacer(repl *strings.Replacer) {
	c.v.SetEnvKeyReplacer(repl)
}

func (c *viperConfig) AutomaticEnv() {
	c.v.AutomaticEnv()
}

func (c *viperConfig) BindEnv(path string, env ...string) error {
	return c.v.BindEnv(append([]string{path}, env...)...)
}
