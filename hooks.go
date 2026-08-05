package lynx

import "context"

// HookFunc 是应用生命周期钩子函数，返回错误时视为钩子执行失败。
type HookFunc func(ctx context.Context) error
