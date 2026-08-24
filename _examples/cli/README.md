# cli 示例

演示 `app.Command` 单次命令执行 + 默认内存 EventBus（`Topic.Publish` / `Subscribe`）的 CLI 应用。

## 运行

```bash
go run . -c config.yaml
```

框架默认启用配置 flag（无需显式开启）：`-c/--config`（配置文件路径）、
`--config-type`（默认 `yaml`）、`--config-dir`、`--log-level`（默认 `info`）。

## 关键代码点

- `main.go`：默认配置 flag 集开箱即用；`zap` 日志写入 `cli.out`。
- `HelloTopic.Subscribe`：Init 期订阅 `hello`（Bus 由框架注入）。
- `app.Command`：命令入口，`HelloTopic.Publish` 后结束。
