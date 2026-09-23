package lynx

// readiness 的唯一归属：单个服务"就绪了吗"的等待机制（Ready 通道 →
// Checker 轮询 → 直接放行）。消费方：OrderedServices 的按序启动、
// newLynx 的总线就绪等待。预算由调用方传入（各自配置），机制只有一份。
//
// 未纳入本模块：OnPostStart 的 startWG 边界（"所有 actor 进入执行体"，
// 文档明确不是 readiness）；command 的依赖等待（v1.12.0 起独立演进为
// 三级就绪探测 + WithProbeTimeout，单次检查经 shutdown.go 的 callBounded
// 限界）；drainChecker（关停信号，见 shutdown.go——它复用 Checker 接口
// 但不表达健康）。

import (
	"context"
	"time"
)

// readinessPollInterval 是 Checker 轮询间隔：就绪无通知机制，忙轮询是
// 唯一手段，10ms 量级兼顾响应与开销。
const readinessPollInterval = 10 * time.Millisecond

// awaitServiceReady 等待单个服务就绪：
//   - 实现 Ready：等待通道关闭（Listen 型服务在 bind 后关闭；Start 已失败
//     则经 startErr 快路径上抛）；
//   - 否则实现 Checker：在 budget 内轮询 CheckHealth（poll 间隔）；
//   - 两者皆无：已 invoke 即继续。
//
// startErr 是该服务 Start 结果的缓冲通道（可为 nil）：非 nil 时全程交错
// 监听，可读即取出、放回（peek 语义，供后续收尾等待仍能收到）并返回。
// Checker 路径预算耗尽时返回 errTimeout(最后一次健康检查错误)。
func awaitServiceReady(ctx context.Context, s Service, budget time.Duration, startErr chan error, errTimeout func(last error) error) error {
	if r, ok := s.(Ready); ok {
		return waitReadyChan(ctx, r.Ready(), startErr)
	}
	if c, ok := s.(Checker); ok {
		return awaitHealthy(ctx, c, budget, readinessPollInterval, startErr, errTimeout)
	}
	// 无 Ready / Checker：已 invoke 即继续。若 Start 已立刻失败则上抛（放回供收尾等待）。
	return peekStartErr(startErr)
}

func waitReadyChan(ctx context.Context, ready <-chan struct{}, startErr chan error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-startErr:
		startErr <- err
		return err
	case <-ready:
		// 成功路径：Ready 关闭后 Start 仍阻塞在 Serve。失败路径不得关闭 Ready。
		return peekStartErr(startErr)
	}
}

// peekStartErr 非阻塞读取 Start 结果并放回，供后续收尾等待仍能收到。
func peekStartErr(startErr chan error) error {
	select {
	case err := <-startErr:
		startErr <- err
		return err
	default:
		return nil
	}
}

// awaitHealthy 在 budget 内轮询 checker 直至 CheckHealth 返回 nil。
// 顺序语义与既有两处轮询逐字对齐：先 peek startErr、再查健康（预算边界
// 上恰好转健康仍算成功）、再判预算，最后在等待槽内交错监听 ctx 取消与
// startErr。startErr 为 nil 时是纯轮询（nil channel 永久阻塞，select 安全）。
// budget <= 0 时至少检查一次。
func awaitHealthy(ctx context.Context, checker Checker, budget, poll time.Duration, startErr chan error, errTimeout func(last error) error) error {
	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var last error
	for {
		if err := peekStartErr(startErr); err != nil {
			return err
		}
		last = checker.CheckHealth()
		if last == nil {
			return peekStartErr(startErr)
		}
		if time.Now().After(deadline) {
			return errTimeout(last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-startErr:
			startErr <- err
			return err
		case <-ticker.C:
		}
	}
}
