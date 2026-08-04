# boot 示例

基于 `boot.Bootstrap` + google/wire 依赖注入组装组件的最小 HTTP 服务示例。

## 运行

```bash
go run . --addr=:8080
```

flag：`--addr`（HTTP 监听地址，默认 `:8080`）、`-l/--loglevel`（日志级别，默认 `debug`）。
`config.yaml` 展示与 `AppConfig` 对应的配置格式（键 `addr`，对应 `mapstructure:"addr"`）。

## 关键代码点

- `main.go:38 NewHttpServer`：构建路由并按 `addr` 配置创建 `server/http.Server`，附带健康检查。
- `config.go AppConfig`：应用配置结构体；`provides.go:25 NewConfig` 通过 `app.Config().Unmarshal(c)` 填充。
- `provides.go:14 ProviderSet`：wire ProviderSet，`wire_gen.go` 由 `//go:generate wire` 生成。
- `main.go:16`：`lynx.NewBuilder` 回调中执行 `wireBootstrap` 并 `boot.Bind(app)`，由 Bootstrap 统一注册组件与生命周期钩子。
- `provides.go:47 NewOnStarts` / `provides.go:56 NewOnStops`：启动 / 停止钩子示例。
