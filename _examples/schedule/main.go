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
	builder := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))
		task1 := &task{}
		app.OnStart(func(ctx context.Context) error {
			return task1.HandlerFunc()(ctx)
		})
		scheduler, err := schedule.NewScheduler([]schedule.Task{task1}, schedule.WithLogger(app.Logger()))
		if err != nil {
			return err
		}
		mux := gohttp.NewServeMux()
		hs := http.NewServer(mux, http.WithAddr(":8089"))
		app.Register(scheduler, hs)
		return nil
	},
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
		//lynx.WithUseDefaultConfigFlagsFunc(),
	)
	builder.Run()
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
