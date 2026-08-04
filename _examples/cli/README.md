# cli 示例

演示 `app.CLI` 单次命令执行 + 内存 pubsub（Broker/Router/Handler）的 CLI 应用。

## 运行

```bash
go run . -c config.yaml
```

使用框架默认配置 flag（`WithUseDefaultConfigFlagsFunc`）：`-c/--config`（配置文件路径）、
`--config-type`（默认 `yaml`）、`--config-dir`、`--log-level`（默认 `info`）。

## 关键代码点

- `main.go:21`：启用默认配置 flag 集，`config.yaml` 提供 `addr` 键。
- `main.go:30-38`：`zap.NewZapLoggerToFile` 将日志写入 `cli.out`，并包装为 slog。
- `main.go:48-57`：`pubsub.NewBroker` + `pubsub.NewRouter` 注册 `helloHandler`，挂为应用组件。
- `main.go:61 app.CLI`：命令入口，发布 `hello` 事件后结束。
- `main.go:73 helloHandler`：实现 `pubsub.Handler` 接口（`EventName`/`HandlerName`/`HandlerFunc`）消费事件。
