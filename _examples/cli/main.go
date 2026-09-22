// cli 示例：使用 github.com/lynx-go/commands 构建子命令式 CLI，
// 在命令的 Run 中启动 lynx 应用（默认内存 Bus 的事件发布/订阅）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/lynx-go/commands"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/eventbus"
)

// HelloTopic 是 CLI 示例的类型化主题（默认内存 Bus）。
var HelloTopic = eventbus.NewTopic[map[string]any]("hello")

type Config struct {
	Addr string `json:"addr"`
}

func main() {
	app := commands.New()
	app.HelpHeader = "cli-example：基于 lynx-go/commands 的子命令式 CLI"
	app.HelpFooter = `使用 "help <命令>" 查看单个命令的用法。`
	app.Register(&helloCmd{}, &versionCmd{})

	env := &commands.Environment{Stdout: os.Stdout, Stderr: os.Stderr}
	os.Exit(app.Run(context.Background(), env, os.Args[1:]))
}

// versionCmd 打印示例版本：commands 的裸命令（无 flags、不依赖 lynx）。
type versionCmd struct{}

func (c *versionCmd) Name() string     { return "version" }
func (c *versionCmd) Synopsis() string { return "打印示例版本" }
func (c *versionCmd) Usage() string    { return "version" }

func (c *versionCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	fmt.Fprintln(env.Stdout, "cli-example v1.7.0")
	return nil
}

// helloCmd 启动 lynx 应用：Init 期订阅 hello 主题，app.Command 里发布一条
// 事件后退出。配置文件路径由 commands 的 -c/--config 传入。
type helloCmd struct {
	configFile string
}

func (c *helloCmd) Name() string { return "hello" }
func (c *helloCmd) Synopsis() string {
	return "启动 lynx 应用，发布一条 hello 事件后退出"
}
func (c *helloCmd) Usage() string { return "hello [-c config.yaml]" }

func (c *helloCmd) SetFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.configFile, "config", "", "配置文件路径")
	fs.StringVar(&c.configFile, "c", "", "配置文件路径（--config 的简写）")
}

func (c *helloCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	return newRunner(c.configFile).RunE()
}

// newRunner 构建 lynx Runner。WithConfigFile 声明参数已由 commands 解析：
// 关闭框架内置的 os.Args 解析，配置文件路径直接绑定（未指定时回退搜索
// 工作目录）——单一选项取代手工组合 WithDisableConfigFlags +
// WithBindConfigFunc（两者顺序敏感，写反会静默丢失绑定）。
func newRunner(configFile string) *lynx.Runner {
	return lynx.NewRunner(func(app lynx.App) error {
		logLevel := app.Config().GetString("log-level")
		if logLevel == "" {
			logLevel = "debug"
		}
		zlogger, err := zap.NewZapLogger(logLevel, "cli.out")
		if err != nil {
			return err
		}
		slogger, err := zap.NewSLogger(zlogger, logLevel)
		if err != nil {
			return err
		}
		app.SetLogger(slogger)

		config := &Config{}
		if err := app.Config().Unmarshal(config); err != nil {
			return err
		}

		logger := app.Logger()
		logger.Info("parsed config", "config", config)

		// 默认内存 Bus 已由框架注入；Init 期订阅即可。
		if err := HelloTopic.Subscribe(app.Context(),
			func(ctx context.Context, e *eventbus.Event[map[string]any]) error {
				slog.InfoContext(ctx, "recv hello event", "payload", e.Payload)
				return nil
			}); err != nil {
			return err
		}

		fmt.Println("hello cli")

		return app.Command(func(ctx context.Context) error {
			if err := HelloTopic.Publish(ctx, map[string]any{"message": "hello world"}); err != nil {
				return err
			}
			logger.Info("command executed successfully")
			return nil
		})
	},
		lynx.WithName("cli-example"),
		lynx.WithConfigFile(configFile),
	)
}
