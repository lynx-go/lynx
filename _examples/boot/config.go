package main

// AppConfig 应用配置，与 config.yaml 对应
type AppConfig struct {
	Addr string `mapstructure:"addr"`
}
