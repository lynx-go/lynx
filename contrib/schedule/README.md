# schedule

```go
import "github.com/lynx-go/lynx/contrib/schedule"
```

基于 robfig/cron 的定时任务调度服务。

## 能力要点

- `Scheduler` 实现 `lynx.Service` 与 `lynx.Checker`（scheduler.go:172-174），服务名 `cron-scheduler`（scheduler.go:82）。
- `Task` 接口（scheduler.go:177-181）：`Name()` / `Cron()` / `HandlerFunc()`；cron 表达式为 6 字段秒级语法，支持 `@every 5s` 描述符。
- `NewScheduler(tasks []Task, opts ...Option) (*Scheduler, error)`（scheduler.go:255）：构造时注册全部任务（Init 不重复注册）；非法 cron 表达式返回错误。
- `Trigger(name string) error`（scheduler.go:369）：按任务名立即触发一次执行，与 cron 触发走完全相同的路径（panic 恢复、Exclusive 互斥、错误上报）；未注册的任务返回 `ErrTaskNotFound`（scheduler.go:251）。Init/Start 前亦可调用（任务上下文回退 Background）。
- `Exclusive(t Task) Task`（exclusive.go:5）：把任务标成集群单例——同一次 cron 格子经 `cluster.TryOnce` 最多一个节点执行；存在 Exclusive 任务但未配 `WithCoordinator` 时 `NewScheduler` 返回 `ErrCoordinatorRequired`（scheduler.go:249）。
- 任务 panic 被恢复并记日志，调度不中断；`HandlerFunc` 返回的错误经 `WithErrorHandler` 回调上报（缺省记 Error 日志），Exclusive 抢锁失败被跳过不算错误。
- 内置默认 cron 实例：秒级解析 + `cron.Recover` + `cron.SkipIfStillRunning`（任务执行超过间隔时不重叠运行，scheduler.go:278-287）。

选项（scheduler.go:191-245）：

| 选项 | 说明 |
| --- | --- |
| `WithLogger(logger *slog.Logger)` | 日志实例；显式设置后 Init 不再用 ctx.Logger 覆盖 |
| `WithCron(cron *cron.Cron)` | 自定义 cron 实例；此时 `WithLocation` 被忽略并记 Warn 日志 |
| `WithDebugEnabled()` | 开启 cron 调试日志输出 |
| `WithLocation(loc *time.Location)` | 任务调度时区，默认 `time.Local` |
| `WithErrorHandler(fn func(ctx context.Context, task Task, err error))` | 任务执行错误回调，缺省记日志 |
| `WithCoordinator(s cluster.Coordinator)` | 进程间协调后端，Exclusive 任务必配 |
| `WithNow(fn func() time.Time)` | Exclusive 互斥格子计算的时间来源（缺省 `time.Now`），供测试注入固定/步进时钟；不影响 cron 引擎触发时序 |

## 快速开始

（取自 `_examples/schedule/main.go`）

```go
package main

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/schedule"
)

type helloTask struct{}

func (t *helloTask) Name() string { return "hello" }
func (t *helloTask) Cron() string { return "@every 5s" }
func (t *helloTask) HandlerFunc() schedule.HandlerFunc {
	return func(ctx context.Context) error {
		slog.InfoContext(ctx, "task triggered")
		return nil
	}
}

func main() {
	lynx.NewRunner(func(app lynx.App) error {
		scheduler, err := schedule.NewScheduler(
			[]schedule.Task{&helloTask{}},
			schedule.WithLogger(app.Logger()),
		)
		if err != nil {
			return err
		}
		app.Register(scheduler)
		return nil
	}, lynx.WithName("schedule-demo")).Run()
}
```

手动触发与集群单例：

```go
// 手动执行一次（测试驱动任务逻辑 / 运维入口），不等待 cron 时序
if err := scheduler.Trigger("hello"); err != nil {
	return err // 未注册的任务返回 ErrTaskNotFound
}

// 集群单例：同一次格子全集群最多一个节点执行
scheduler, err := schedule.NewScheduler([]schedule.Task{
	localRefresh,                       // 每节点各自触发
	schedule.Exclusive(nightlyBilling), // 全集群一次
}, schedule.WithCoordinator(coord))
```

## 与 lynx 核心的集成

- `app.Register(scheduler)` 挂载生命周期：Init 取 `ctx.Context()` 作为任务执行上下文（携带应用元数据，应用关闭时取消）并复用 `ctx.Logger`（显式 `WithLogger` 时不覆盖）；Start 阻塞至应用关闭；Stop 等待在途任务收敛（无 deadline 时立即返回，由框架的 `StopTimeout` 统一兜底，scheduler.go:152-170）。
- `CheckHealth()`（scheduler.go:71）：调度器未运行（Start 前 / Stop 后）返回错误；实现 `lynx.Checker`，注册后自动进入健康检查聚合（`app.HealthCheckers` → HTTP readiness）。
- `WithCoordinator` 接 `contrib/cluster` 的 `Coordinator`（内存 / consul / redis 后端），跨进程互斥与 `cluster.Singleton` 整调度器单节点运行见 `docs/04-service-system.md` 的 cluster 一节。

## 相关文档与示例

- `docs/04-service-system.md` 4.5 节「schedule：定时任务（Scheduler/Task）」「cluster：进程间协调」
- `_examples/schedule/`：完整示例（调度器与 HTTP 服务多服务共存），含 `README.md`
