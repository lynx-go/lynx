// cli 示例：多命令 + Wire（boot）组合。
//
// 演示 v1.12.0 的 CLI 模式三件套：
//   - 子命令调度由 lynx-go/commands 承担，参数（含 -c/--config）由其
//     解析，lynx.WithConfigFile 声明配置路径并关闭框架内置 flags；
//   - Wire 图返回双聚合 App{*boot.Bootstrap; *Deps}（app.go）：Bootstrap
//     管框架要托管的（Store 服务的生命周期 + hooks），Deps 管命令要
//     调用的（类型化业务对象）——命令选择留在图外，多命令扩展线性；
//   - runLynx helper（runner.go）缝合两者：新增命令 = 新 cmd 类型 +
//     一个函数，Wire 图与 helper 零改动。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/lynx-go/commands"
)

func main() {
	app := commands.New()
	app.HelpHeader = "cli 示例：多命令 + Wire 双聚合（boot 模式）"
	app.HelpFooter = `使用 "help <命令>" 查看单个命令的用法。`
	app.Register(&versionCmd{}, &setCmd{}, &getCmd{}, &listCmd{})

	env := &commands.Environment{Stdout: os.Stdout, Stderr: os.Stderr}
	os.Exit(app.Run(context.Background(), env, os.Args[1:]))
}

// versionCmd 打印示例版本：commands 的裸命令（无 flags、不启动 lynx）——
// 不是每个子命令都需要拉起框架。
type versionCmd struct{}

func (c *versionCmd) Name() string             { return "version" }
func (c *versionCmd) Synopsis() string         { return "打印示例版本（裸命令，不启动 lynx）" }
func (c *versionCmd) Usage() string            { return "version" }
func (c *versionCmd) SetFlags(_ *flag.FlagSet) {}
func (c *versionCmd) Run(_ context.Context, env *commands.Environment, _ []string) error {
	_, _ = fmt.Fprintln(env.Stdout, "cli-example v1.12.0")
	return nil
}

// setCmd 写入键值对：经 runLynx 拉起 Wire 图，Store 在优雅关停的 Stop
// 里落盘——演示"命令完成 → 框架关停 → 服务 Stop"的完整生命周期。
type setCmd struct {
	configFile string
}

func (c *setCmd) Name() string { return "set" }
func (c *setCmd) Synopsis() string {
	return "写入键值对（经 Wire 图的 Store，关停时落盘）"
}
func (c *setCmd) Usage() string { return "set <key> <value> [-c config.yaml]" }
func (c *setCmd) SetFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.configFile, "config", "", "配置文件路径")
	fs.StringVar(&c.configFile, "c", "", "配置文件路径（--config 的简写）")
}
func (c *setCmd) Run(_ context.Context, env *commands.Environment, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("set 需要 <key> <value> 两个参数")
	}
	key, value := args[0], args[1]
	return runLynx(c.configFile, func(_ context.Context, a *App) error {
		a.Store.Set(key, value)
		_, _ = fmt.Fprintf(env.Stdout, "set %s=%s\n", key, value)
		return nil
	})
}

// getCmd 读取键值对：与 setCmd 共享同一个 Wire 图与 helper，只有 fn 不同。
type getCmd struct {
	configFile string
}

func (c *getCmd) Name() string     { return "get" }
func (c *getCmd) Synopsis() string { return "读取键值对（图在构造期加载 store 文件）" }
func (c *getCmd) Usage() string    { return "get [-c config.yaml] <key>" }
func (c *getCmd) SetFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.configFile, "config", "", "配置文件路径")
	fs.StringVar(&c.configFile, "c", "", "配置文件路径（--config 的简写）")
}
func (c *getCmd) Run(_ context.Context, env *commands.Environment, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("get 需要 <key> 一个参数")
	}
	key := args[0]
	return runLynx(c.configFile, func(_ context.Context, a *App) error {
		value, ok := a.Store.Get(key)
		if !ok {
			return fmt.Errorf("key %q not found", key)
		}
		_, _ = fmt.Fprintf(env.Stdout, "%s=%s\n", key, value)
		return nil
	})
}

// listCmd 列出全部键：第三个复用同一图的命令——多命令扩展的线性成本。
type listCmd struct {
	configFile string
}

func (c *listCmd) Name() string     { return "list" }
func (c *listCmd) Synopsis() string { return "列出全部键值对" }
func (c *listCmd) Usage() string    { return "list [-c config.yaml]" }
func (c *listCmd) SetFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.configFile, "config", "", "配置文件路径")
	fs.StringVar(&c.configFile, "c", "", "配置文件路径（--config 的简写）")
}
func (c *listCmd) Run(_ context.Context, env *commands.Environment, _ []string) error {
	return runLynx(c.configFile, func(_ context.Context, a *App) error {
		for _, key := range a.Store.Keys() {
			value, _ := a.Store.Get(key)
			_, _ = fmt.Fprintf(env.Stdout, "%s=%s\n", key, value)
		}
		return nil
	})
}
