package lynx

import "sync/atomic"

var defaultAppContext atomic.Pointer[appContextHolder]

type appContextHolder struct{ c AppContext }

// Set 设置进程默认 AppContext（类比 slog.SetDefault / eventbus.SetDefault）。
// newLynx 成功后调用；测试可在结束后 Set(nil) 清理。
func Set(ctx AppContext) {
	if ctx == nil {
		defaultAppContext.Store(nil)
		return
	}
	defaultAppContext.Store(&appContextHolder{c: ctx})
}

// Get 返回 Set 设置的 AppContext；未设置时返回 nil。
func Get() AppContext {
	h := defaultAppContext.Load()
	if h == nil {
		return nil
	}
	return h.c
}
