package cluster

import (
	"context"
	"time"
)

// TryOnce 尝试 Claim name：抢到则执行 fn 且不 Release（占位靠 ttl 过期）；
// 未抢到返回 skipped=true。Claim 出错时不执行 fn。
func TryOnce(ctx context.Context, s Store, name string, ttl time.Duration, fn func(context.Context) error) (skipped bool, err error) {
	if s == nil {
		return false, ErrNilStore
	}
	won, err := s.Claim(ctx, name, ttl)
	if err != nil {
		return false, err
	}
	if !won {
		return true, nil
	}
	return false, fn(ctx)
}
