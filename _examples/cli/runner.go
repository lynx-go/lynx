package main

import (
	"context"

	"github.com/lynx-go/lynx"
)

// runLynx 是整个二进制写一次的 helper：图供依赖（wireApp 双聚合），
// 子命令供函数（fn），WithConfigFile 声明参数已由 commands 解析。
// 新增命令 = 新 cmd 类型 + 一个函数，本函数与 Wire 图零改动。
func runLynx(configFile string, fn func(ctx context.Context, a *App) error) error {
	return lynx.NewRunner(func(app lynx.App) error {
		a, cleanup, err := wireApp(app)
		if err != nil {
			return err
		}
		// Wire cleanup 挂终局阶段：所有服务 Stop、总线关停之后执行
		// （同 _examples/boot，勿放 OnPreStop——在途请求还用资源）。
		app.OnPostStop(cleanup)
		// Store 等服务注册：Init 同步执行，Start/Stop 归框架托管；
		// 命令起跑前经三级就绪解析等待它们就绪。
		a.Apply(app)
		return app.Command(func(ctx context.Context) error { return fn(ctx, a) })
	}, lynx.WithConfigFile(configFile)).RunE()
}
