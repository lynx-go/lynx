# cli 示例：多命令 + Wire 双聚合（boot 模式）

使用 [`github.com/lynx-go/commands`](https://github.com/lynx-go/commands)
构建多子命令 CLI，命令的业务对象来自 Wire 依赖图。演示 v1.12.0 的
CLI 模式三件套：`lynx.WithConfigFile`（配置桥接）、双聚合注入器出口
（`App{*boot.Bootstrap; *Deps}`）、`runLynx` helper（图供依赖、子命令
供函数）。

## 运行

```bash
go run . help                  # 打印帮助
go run . version               # 裸命令：打印版本（不依赖 lynx）
go run . set -c config.yaml a 1   # 写入键值对（Store 在关停的 Stop 里落盘）
go run . get -c config.yaml a     # 读取（图在构造期加载 store 文件）
go run . list -c config.yaml      # 列出全部
```

注意 flags 在位置参数之前（标准库 `flag` 在首个位置参数处停止解析）。
省略 `-c` 时回退搜索工作目录（`WithConfigFile` 空路径语义）。

## 结构与关键代码点

- `main.go`：`commands.New` + `Register` 注册四个子命令，`app.Run`
  返回进程退出码（0 成功 / 1 命令错误 / 2 用法错误）。`version` 是
  不启动 lynx 的裸命令；`set`/`get`/`list` 各自只提供一个
  `func(ctx, *App) error`，经 `runLynx` 拉起同一个 Wire 图。
- `app.go`：**双聚合出口** `App{*boot.Bootstrap; *Deps}`——Bootstrap
  管框架要托管的（Store 服务的生命周期 + hooks），Deps 管命令要调用
  的（类型化业务对象），同一批 Wire 单例两个视图。命令选择留在图外
  （运行时由子命令框架决定），新增命令不触碰图。
- `runner.go`：**runLynx helper**，整个二进制写一次：
  `wireApp` 取双聚合 → Wire cleanup 挂 `OnPostStop`（终局阶段）→
  `a.Apply(app)` 注册服务 → `app.Command(fn)`。新增命令 = 新 cmd
  类型 + 一个函数，helper 与图零改动。
- `provides.go`：`Store` 是文件支撑的 KV 服务（`Service` + `Checker`）——
  命令起跑前经三级就绪解析等它就绪；`Stop` 在优雅关停时把内存态
  落盘，演示"命令完成 → 框架关停 → 服务 Stop"的完整生命周期。
- `lynx.WithConfigFile(c.configFile)`：声明参数已由 commands 解析——
  关闭框架内置的 `os.Args` 解析并把配置路径直接绑定（空路径回退搜索
  工作目录）。单一选项取代顺序敏感的手工组合 `WithDisableConfigFlags`
  + `WithBindConfigFunc`（写反会静默丢失绑定）。

## 扩展一个新命令

加一个 cmd 类型 + 一个函数即可，Wire 图、`runLynx`、既有命令零改动：

```go
type delCmd struct{ configFile string }
// ... Name/Synopsis/Usage/SetFlags 同 setCmd ...
func (c *delCmd) Run(_ context.Context, env *commands.Environment, args []string) error {
	return runLynx(c.configFile, func(_ context.Context, a *App) error {
		return a.Store.Delete(args[0])
	})
}
```
