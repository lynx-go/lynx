package cluster

// 租约引擎：Claim/Acquire 的公共骨架（入参校验、续约间隔、续约循环）
// 在此唯一归属，三个后端适配器（memory / cluster-redis / consul）共用；
// 适配器只保留真正后端相关的部分——存储、锁脚本、Session。
// TTL 下限是适配器能力差异（Consul Session 最短 10s），经可选能力
// TTLAware 声明、cluster.MinTTL 查询，不拓宽 Coordinator 核心接口。

import (
	"context"
	"time"
)

// ValidateCall 校验 Claim/Acquire 的公共入参，返回第一个命中的错误：
// ctx 已取消 → ctx.Err()；name 空 → ErrEmptyName；ttl 非正 → ErrInvalidTTL。
// 适配器在触达后端前调用，保证三个后端的行为一致。
func ValidateCall(ctx context.Context, name string, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" {
		return ErrEmptyName
	}
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	return nil
}

// RenewInterval 返回租约续约间隔：ttl/3（下限 1ms 防止零值 ticker panic）。
// Consul 侧 ttl 已被 sessionTTL 抬到 ≥10s，1ms 下限不生效。
func RenewInterval(ttl time.Duration) time.Duration {
	return max(ttl/3, time.Millisecond)
}

// RunRenewLoop 是租约续约循环的共享骨架：每 interval 触发一次 renew，
// renew 返回非 nil 即视为租约丢失——cancel（Lease.Context 随之取消）并
// 退出。renew 自行决定是否携带租约 ctx（Redis 续约刻意用 Background：
// 续约不受调用侧取消影响，仅由丢失退出）。阻塞调用，适配器以
// `go RunRenewLoop(...)` 启动。
func RunRenewLoop(ctx context.Context, cancel context.CancelFunc, interval time.Duration, renew func() error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := renew(); err != nil {
				cancel()
				return
			}
		}
	}
}

// TTLAware 是 Coordinator 的可选能力：声明租约 TTL 下限（无下限返回 0）。
// Consul Session 最短 10s（MinSessionTTL）；内存 / Redis 无下限。
type TTLAware interface {
	// MinTTL 返回该后端接受的最小租约 TTL。
	MinTTL() time.Duration
}

// MinTTL 返回协调后端的租约 TTL 下限：实现 TTLAware 时取其声明，
// 否则 0（无下限）。调用方（如 schedule 的 Exclusive 触发）在计算短 TTL
// 前查询并钳制，避免每次触发都撞后端下限报错。
func MinTTL(c Coordinator) time.Duration {
	if t, ok := c.(TTLAware); ok && t != nil {
		return t.MinTTL()
	}
	return 0
}
