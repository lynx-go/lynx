package lynx

import (
	"context"
	"fmt"
	"os"
)

// SetupFunc 是应用初始化回调，在 CLI 运行前执行，用于注册组件与 hooks。
type SetupFunc func(ctx context.Context, app Lynx) error

// CLI 是 Lynx 应用的命令行入口，封装应用实例与初始化回调。
type CLI struct {
	setup SetupFunc
	lynx  Lynx
}

// New 创建 CLI 实例；初始化失败时输出错误并退出进程。
func New(o *Options, setup SetupFunc) *CLI {
	app, err := newLynx(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return &CLI{
		setup: setup,
		lynx:  app,
	}
}

// Run 运行 CLI 应用，发生错误时输出到 stderr 并以非零状态码退出进程。
func (app *CLI) Run() {
	if err := app.RunE(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// RunE 运行 CLI 应用并返回错误，由调用方决定错误处理方式。
func (app *CLI) RunE() error {
	if err := app.setup(app.lynx.Context(), app.lynx); err != nil {
		return err
	}
	return app.lynx.Run()
}
