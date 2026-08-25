package lynx

import "sync/atomic"

var defaultAppContext atomic.Pointer[appContextHolder]

type appContextHolder struct{ c AppContext }

// Set 设置进程默认 AppContext（类比 slog.SetDefault / eventbus.SetDefault）。
// newLynx 成功后调用。
//
// 单 App 进程假设：该全局槽只有一个，多 App 并存（典型是测试）时后设置
// 者覆盖前者。测试清理模式：用例结束后 Set(nil) 复位，避免泄漏到同进程
// 的后续用例（与 eventbus.SetDefault 的清理模式一致）。
func Set(ctx AppContext) {
	if ctx == nil {
		defaultAppContext.Store(nil)
		return
	}
	defaultAppContext.Store(&appContextHolder{c: ctx})
}

// Get 返回 Set 设置的 AppContext；未设置时返回 nil。
// 单 App 进程假设同 Set：返回的是最近一次 Set 的实例，多 App 测试场景
// 下不保证属于当前用例——需要确定归属时优先经由依赖注入传递 App，
// 而非依赖本全局槽。
func Get() AppContext {
	h := defaultAppContext.Load()
	if h == nil {
		return nil
	}
	return h.c
}
