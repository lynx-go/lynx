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

- `main.go:35`：注册 `metrics.New()`（`contrib/metrics`）——以**组件**形式托管 OTel（Init 创建 provider 并设为全局，Stop 自动 flush）。
- `metrics.go:16 initMetrics`：创建业务指标（Counter + Histogram），挂在全局 MeterProvider 上。
- `main.go:58`：`/` 处理器中 `helloRequestsCounter.Add` 计数、`helloRequestDuration.Record` 记录耗时。
- `main.go:78`：`/metrics` 暴露 Prometheus 指标（演示用，挂在主路由上）——自定义指标与运行时指标一起导出。
- `main.go:80-88`：`http.NewServer` 注册 HTTP 服务；otel provider 走全局（OTel 组件设置），无需逐项传入。
- `main.go:98-115`：自定义 flag 与 `WithBindConfigFunc`，启用 `LYNX_` 环境变量前缀并绑定 `LYNX_ADDR`。
- `main.go:120 latencyMiddleware`：记录请求耗时的自定义中间件示例。
