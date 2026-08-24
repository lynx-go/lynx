package eventbus

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrNoBus 表示无法解析出 Bus（无 WithBus、Context 未注入且未 SetDefault）。
var ErrNoBus = errors.New("eventbus: no bus (use eventbus.WithBus, ContextWithBus, or SetDefault)")

type busCtxKey struct{}

var defaultBus atomic.Pointer[busHolder]

type busHolder struct{ b Bus }

// SetDefault 设置进程默认 Bus（类比 slog.SetDefault）。
// newLynx 在 Bus.Init 成功后调用；测试可在结束后 SetDefault(nil) 清理。
func SetDefault(b Bus) {
	if b == nil {
		defaultBus.Store(nil)
		return
	}
	defaultBus.Store(&busHolder{b: b})
}

// Default 返回 SetDefault 设置的 Bus；未设置时返回 nil。
func Default() Bus {
	h := defaultBus.Load()
	if h == nil {
		return nil
	}
	return h.b
}

// ContextWithBus 将 Bus 写入 ctx，供 Topic.Publish/Subscribe 解析。
func ContextWithBus(ctx context.Context, b Bus) context.Context {
	return context.WithValue(ctx, busCtxKey{}, b)
}

// BusFromContext 从 ctx 取 Bus；未设置时返回 nil。
func BusFromContext(ctx context.Context) Bus {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(busCtxKey{}).(Bus)
	return b
}

// resolveBus 按已拍板顺序解析：显式 override → Context → Default。
func resolveBus(ctx context.Context, override Bus) (Bus, error) {
	if override != nil {
		return override, nil
	}
	if b := BusFromContext(ctx); b != nil {
		return b, nil
	}
	if b := Default(); b != nil {
		return b, nil
	}
	return nil, ErrNoBus
}
