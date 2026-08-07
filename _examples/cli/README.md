# cli 示例

演示 `app.Command` 单次命令执行 + 内存 pubsub（Broker/Router/Handler）的 CLI 应用。

## 运行

```bash
go run . -c config.yaml
```

框架默认启用配置 flag（无需显式开启）：`-c/--config`（配置文件路径）、
`--config-type`（默认 `yaml`）、`--config-dir`、`--log-level`（默认 `info`）。

## 关键代码点

- `main.go:18`：默认配置 flag 集开箱即用，`config.yaml` 已清空（配置全部走代码显式构造）。
- `main.go:24`：`zap.NewZapLogger(logLevel, "cli.out")` 将日志写入 `cli.out`，并包装为 slog。
- `main.go:42-53`：`pubsub.NewBroker` + `pubsub.NewRouter` 注册 `helloHandler`，挂为应用服务。
- `main.go:54 app.Command`：命令入口，发布 `hello` 事件后结束。
- `main.go:45`：`pubsub.NewHandler` 构造原始字节 handler 消费 `hello` 事件。
