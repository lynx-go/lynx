package lynx

import "github.com/spf13/viper"

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
	// Unmarshal 将配置解码到 out 指向的结构体。
	Unmarshal(out any) error
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

func (c *viperConfig) Unmarshal(out any) error {
	return c.v.Unmarshal(out)
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

func (c *viperConfig) AutomaticEnv() {
	c.v.AutomaticEnv()
}

func (c *viperConfig) BindEnv(path string, env ...string) error {
	return c.v.BindEnv(append([]string{path}, env...)...)
}
