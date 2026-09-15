package lynx

import "context"

// HookFunc 是应用生命周期钩子函数，返回错误时视为钩子执行失败。
type HookFunc func(ctx context.Context) error

// CleanupFunc 是进程收尾清理回调，签名为 func()：终局阶段（一切已停止）
// 错误没有消费者，实现方自行记日志。与 Wire 生成的 cleanup 函数签名
// 原生对齐，app.OnPostStop(cleanup) 零适配。
type CleanupFunc func()
