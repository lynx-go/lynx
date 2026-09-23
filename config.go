package lynx

import (
	"errors"
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
	// Unmarshal 将配置解码到 out 指向的结构体。结构体目标默认走结构体驱动的
	// 逐叶取值：tag 回退链 mapstructure → json → 小写字段名，仅在环境变量
	// 设置的键（配置文件无此键）也参与解码；非结构体目标（map 等，无法
	// 枚举叶子）回落 viper 语义。通过 opts 选择或定制解码行为（见 UnmarshalOption）。
	Unmarshal(out any, opts ...UnmarshalOption) error
	// UnmarshalKey 将 path 对应的配置子树解码到 out 指向的结构体；
	// opts 语义与 Unmarshal 相同，叶子键以 path 为前缀。
	UnmarshalKey(path string, out any, opts ...UnmarshalOption) error
}

// UnmarshalOptions 是 Unmarshal/UnmarshalKey 的解码设置。
// 字段均为解码器无关概念，任意 Config 实现都应能解释。
type UnmarshalOptions struct {
	// TagName 非空时仅按该 struct tag 匹配配置键（无 tag 的字段按
	// 小写字段名匹配）。空值按 mapstructure → json → 小写字段名回退。
	TagName string
	// StrictTypes 拒绝非字符串标量到集合的弱类型转换（如 brokers: 42 被
	// 静默弱转为 []string{"42"}）：启动期报错优于静默错值。字符串来源的
	// 弱转不受影响——环境变量注入的值恒为字符串（"a,b" → 切表、"42" →
	// int 均为合法弱转），拒绝字符串会使 env 覆盖不可用。
	StrictTypes bool
	// ErrorUnused 报告配置子树中未被目标结构体消费的键（未知键，多为
	// 拼写错误）。仅 UnmarshalKey 支持（需要可枚举的配置子树，Unmarshal
	// 传入时返回明确错误）；动态键的 map 字段整体视为已消费。
	ErrorUnused bool
}

// UnmarshalOption 定制 Unmarshal/UnmarshalKey 的解码行为。
type UnmarshalOption func(*UnmarshalOptions)

// WithTagName 指定匹配配置键所用的 struct tag（缺 tag 字段按小写字段名匹配）。
func WithTagName(tag string) UnmarshalOption {
	return func(o *UnmarshalOptions) { o.TagName = tag }
}

// WithStrictTypes 拒绝非字符串标量到集合的弱类型转换（如 brokers: 42
// 弱转为 []string{"42"} 的静默错配）；字符串来源（环境变量注入）不受影响。
func WithStrictTypes() UnmarshalOption {
	return func(o *UnmarshalOptions) { o.StrictTypes = true }
}

// WithErrorUnused 报告配置子树中未被结构体消费的未知键（仅支持 UnmarshalKey）。
func WithErrorUnused() UnmarshalOption {
	return func(o *UnmarshalOptions) { o.ErrorUnused = true }
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
	if o.ErrorUnused {
		// Config 接口无全量配置树枚举（无 AllSettings），未知键检测仅对
		// 可定位的子树（UnmarshalKey）有意义；静默忽略选项即接口说谎。
		return errors.New("lynx: WithErrorUnused 仅支持 UnmarshalKey（Unmarshal 无法枚举全量配置树）")
	}
	if structTypeOf(out) != nil {
		return unmarshalByStruct(c, "", out, o)
	}
	return c.v.Unmarshal(out, o.viperOpts()...)
}

func (c *viperConfig) UnmarshalKey(path string, out any, opts ...UnmarshalOption) error {
	o := applyUnmarshalOptions(opts)
	if structTypeOf(out) != nil {
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
