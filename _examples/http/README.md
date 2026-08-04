# http 示例

带 OpenTelemetry（stdout trace + Prometheus metrics）、自定义中间件与环境变量绑定的 HTTP 服务示例。

## 运行

```bash
go run . --addr=:8080
# 或通过环境变量指定监听地址
LYNX_ADDR=:8080 go run .
```

flag：`-c/--config`（配置文件路径，默认 `./configs`）、`--addr`（HTTP 监听地址）、`-l/--log_level`（日志级别，默认 `debug`）。
环境变量前缀为 `LYNX_`，`addr` 显式绑定 `LYNX_ADDR`。

## 关键代码点

- `main.go:24-41`：自定义 flag 与 `WithBindConfigFunc`，启用 `LYNX_` 环境变量前缀并绑定 `LYNX_ADDR`。
- `otel.go:18 setupOTel`：初始化 stdout trace exporter 与 Prometheus meter provider，shutdown 挂到 `OnStop`。
- `main.go:97`：`/metrics` 暴露 Prometheus 指标（演示用，挂在主路由上）。
- `main.go:99-107`：`http.NewServer` 注入 TracerProvider/MeterProvider/Propagator 与 `latencyMiddleware`。
- `main.go:124 latencyMiddleware`：记录请求耗时的自定义中间件示例。
