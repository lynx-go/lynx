package lynx

import "time"

// Clock 是框架模块的时间源接缝：生产使用 internal/clock.Real()，测试可
// 注入可控时钟（internal/clock.Fake）以确定性断言 TTL / 过期 / stale 边界，
// 而不是 sleep 真实时间。消费方：contrib/cluster 的租约续约与内存协调器、
// contrib/registry 的缓存 stale 判定。
type Clock interface {
	Now() time.Time
	// After 在 d 之后向返回的 channel 发送当前时间（语义同 time.After）。
	After(d time.Duration) <-chan time.Time
}
