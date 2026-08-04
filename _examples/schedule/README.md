# schedule 示例

cron 定时任务调度示例：任务每 5 秒触发一次，附带一个 HTTP 服务演示多组件共存。

## 运行

```bash
go run .
```

启动后每 5 秒在日志输出一次 `task triggered`；HTTP 服务监听 `:8089`。

## 关键代码点

- `main.go:29 schedule.NewScheduler`：注册任务列表，调度器作为组件挂载生命周期。
- `main.go:40 task`：实现 `schedule.Task` 接口（`Name`/`Cron`/`HandlerFunc`）。
- `main.go:48 Cron`：`@every 5s` 定义触发频率。
- `main.go:26-28`：`OnStart` 钩子中先手动执行一次任务。
- `main.go:33-35`：空路由 HTTP 服务（`:8089`）与调度器一起作为组件运行。
