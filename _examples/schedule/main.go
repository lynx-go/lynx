package main

import (
	"context"
	gohttp "net/http"
	"os"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/schedule"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/lynx-go/x/log"
	"github.com/samber/lo"
)

func main() {
	options := lynx.NewOptions(
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
		//lynx.WithUseDefaultConfigFlagsFunc(),
	)

	cli := lynx.New(options, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.MustNewLogger(app))
		task1 := &task{}
		_ = app.Hook(lynx.OnStart(func(ctx context.Context) error {
			return task1.HandlerFunc()(ctx)
		}))
		scheduler, err := schedule.NewScheduler([]schedule.Task{task1}, schedule.WithLogger(app.Logger()))
		if err != nil {
			return err
		}
		mux := gohttp.NewServeMux()
		hs := http.NewServer(mux, http.WithAddr(":8089"))
		return app.Hook(lynx.Components(scheduler, hs))
	})
	cli.Run()
}

type task struct {
}

func (t *task) Name() string {
	return "TaskExample"
}

func (t *task) Cron() string {
	return "@every 5s"
}

func (t *task) HandlerFunc() schedule.HandlerFunc {
	return func(ctx context.Context) error {
		log.InfoContext(ctx, "task triggered")
		return nil
	}
}

var _ schedule.Task = new(task)
