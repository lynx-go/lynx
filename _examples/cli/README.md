# cli 示例

使用 [`github.com/lynx-go/commands`](https://github.com/lynx-go/commands) 构建
子命令式 CLI，并在命令的 `Run` 中启动 lynx 应用，演示默认内存 EventBus
（`Topic.Publish` / `Subscribe`）的事件发布/订阅。

## 运行

```bash
go run . help            # 打印帮助
go run . version         # 裸命令：打印版本（不依赖 lynx）
go run . hello           # 启动 lynx 应用，发布一条 hello 事件后退出
go run . hello -c config.yaml
```

## 关键代码点

- `main.go`：`commands.New` + `Register` 注册子命令，`app.Run` 返回进程退出码
  （0 成功 / 1 命令错误 / 2 用法错误）。
- `helloCmd`：在 commands 的 `Run` 里经 `newRunner` 启动 lynx；
  `SetFlags` 用标准库 `flag` 声明 `-c/--config`。
- `newRunner`：`lynx.WithDisableConfigFlags()` 关闭框架内置的 `os.Args`
  解析（参数已由 commands 解析），`WithBindConfigFunc` 把 commands 传来的
  配置路径绑定到配置源；未指定时搜索工作目录。
- `HelloTopic.Subscribe`：Init 期订阅 `hello`（Bus 由框架注入）；
  `app.Command` 里 `HelloTopic.Publish` 后结束。
