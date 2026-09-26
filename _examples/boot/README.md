# boot 示例

基于 `boot.Bootstrap` + google/wire 依赖注入组装服务的最小 HTTP 服务示例。

## 运行

```bash
go run . -c config.yaml --addr=:8080
# 或仅用 flag 默认值
go run . --addr=:8080
```

flag：`-c/--config`（配置文件路径）、`--addr`（HTTP 监听地址，默认 `:8080`）、`-l/--loglevel`（日志级别覆盖，如 `-l debug`；未传时取配置 `logging.level`，框架缺省 info）。
`config.yaml` 与 `AppConfig` 对应（键 `addr`，对应 `mapstructure:"addr"`）；`--addr` flag 会覆盖配置文件中的值。

自定义 flag（如 `-l/--loglevel`）要影响框架读取的配置键，必须在 `WithBindConfigFunc` 里显式翻译进规范键（`c.Set("logging.level", lv)`）——框架只自动翻译内置 `--log-level`，模式见 `lynx` 包 `initConfigure` 与本示例 `main.go`；漏掉翻译时 flag 值只是躺在 viper 里的死键，不报错也不生效。

## 关键代码点

- `main.go:43 NewHttpServer`：构建路由并按 `addr` 配置创建 `server/http.Server`，附带健康检查。
- `config.go AppConfig`：应用配置结构体；`provides.go:24 NewConfig` 通过 `app.Config().Unmarshal(c)` 填充。
- `provides.go:14 ProviderSet`：wire ProviderSet，`wire_gen.go` 由 `//go:generate wire` 生成。
- `main.go:16`：`lynx.NewRunner` 回调中执行 `wireBootstrap` 并 `boot.Apply(app)`，由 Bootstrap 统一注册服务与生命周期钩子。
- `provides.go:40 NewOnStarts` / `provides.go:49 NewOnStops`：启动 / 停止钩子示例。
