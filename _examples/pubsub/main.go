package main

import (
	"context"
	"os"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/samber/lo"
)

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))

		// Wire 依赖注入生成 bootstrap（provides.go 的 ProviderSet 定义
		// kafka/pubsub/http 各服务 provider，配置全部来自 config.yaml）。
		bootstrap, cleanup, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		app.OnStop(func(ctx context.Context) error {
			cleanup()
			return nil
		})
		bootstrap.Bind(app)
		return nil
	},
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
	)
	runner.Run()
}
