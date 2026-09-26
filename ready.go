package lynx

// readiness 的唯一归属：单个服务"就绪了吗"的等待机制——三级解析
//（Ready 通道 → Checker 轮询 → 直接放行）、两种消费模式（单次有界探测、
// 预算内有界循环）与全部有界原语。消费方：OrderedServices 的按序启动、
// newLynx 的总线就绪等待、Command 的依赖探测。
//
// 不变量：任何探测调用（Ready 等待、CheckHealth 调用）都不得越过调用方
// 给出的预算；预算耗尽按"未就绪"处理，由消费方决定重试还是失败。
//
// 未纳入本模块：OnPostStart 的 startWG 边界（"所有 actor 进入执行体"，
// 文档明确不是 readiness）；drainChecker（关停信号，复用 Checker 接口但
// 不表达健康）。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// readinessPollInterval 是 Checker 轮询间隔：就绪无通知机制，忙轮询是
// 唯一手段，10ms 量级兼顾响应与开销。
const readinessPollInterval = 10 * time.Millisecond

// defaultProbeTimeout 是单次就绪探测的默认上界（一次 CheckHealth 或一次
// Ready 等待），也是预算循环在"总预算已耗尽仍需至少检查一次"时的调用
// 兜底上界。Command 的 WithProbeTimeout 以此为默认值。
const defaultProbeTimeout = 3 * time.Second

// probe 是单个服务就绪来源的解析产物与探测执行器（三级解析与两种消费
// 模式的唯一归属）：Ready 优先（即使同时实现 Checker）→ Checker 轮询 →
// 两者皆无表示「已 invoke 即就绪」。startErr 交错、ctx 取消优先与预算
// 边界都在此统一，消费方（OrderedServices / 总线等待 / Command）只选
// wait（预算循环）或 once（单次探测）。
type probe struct {
	ready   <-chan struct{}
	checker Checker
}

// resolveProbe 解析单个服务的就绪来源（三级优先，见 probe 注释）。
func resolveProbe(s Service) probe {
	if r, ok := s.(Ready); ok {
		return probe{ready: r.Ready()}
	}
	if c, ok := s.(Checker); ok {
		return probe{checker: c}
	}
	return probe{}
}

// checkerProbe 构造 Checker-only 探测（总线等非 Service 的 Checker）。
func checkerProbe(c Checker) probe { return probe{checker: c} }

// empty 报告无就绪信号（两者皆无）：消费方视为已就绪、无需探测。
func (p probe) empty() bool { return p.ready == nil && p.checker == nil }

// once 执行单次有界探测（Command 每轮的消费模式）：Ready 有界等待
// （预算内未关闭返回 errReadyNotClosed，由调用方按「本轮未就绪」处理）；
// Checker 单次有界健康检查；无信号返回 nil。ctx 取消优先于探测结果。
func (p probe) once(ctx context.Context, perCall time.Duration) error {
	switch {
	case p.ready != nil:
		return waitReadyBounded(ctx, p.ready, perCall, nil)
	case p.checker != nil:
		return checkHealthBounded(ctx, p.checker, perCall)
	default:
		return nil
	}
}

// wait 在预算内等待就绪（OrderedServices / 总线就绪的消费模式）：Ready
// 有界等待；Checker 有界轮询；无信号 peek Start 结果后放行。
//
// startErr 是该服务 Start 结果的缓冲通道（可为 nil）：非 nil 时全程交错
// 监听，可读即取出、放回（peek 语义，供后续收尾等待仍能收到）并返回；
// 读到的 nil（服务在等待期间正常收尾）视为就绪，与 ctx 取消同时发生时
// 取消为权威裁决。预算耗尽时返回 errTimeout(最后一次探测错误)；errTimeout
// 为 nil 时透传原错误。
func (p probe) wait(ctx context.Context, budget time.Duration, startErr chan error, errTimeout func(last error) error) error {
	if errTimeout == nil {
		errTimeout = func(err error) error { return err }
	}
	switch {
	case p.ready != nil:
		err := waitReadyBounded(ctx, p.ready, budget, startErr)
		if errors.Is(err, errReadyNotClosed) {
			return errTimeout(err)
		}
		return err
	case p.checker != nil:
		return awaitHealthy(ctx, p.checker, budget, readinessPollInterval, startErr, errTimeout)
	default:
		return peekStartErr(startErr)
	}
}

// errReadyNotClosed 标记 Ready 通道在预算内未关闭：循环模式据此交给
// errTimeout 包装（与 Checker 路径的预算耗尽语义对齐），单次探测模式
// 直接返回给消费方（Command 按"本轮未就绪"参与退避重试）。
var errReadyNotClosed = errors.New("ready signal not closed")

// waitReadyBounded 在 timeout 内等待 Ready 通道关闭：
//   - 已闭合：立即放行（即使 ctx 已取消——"首查即就绪即运行"）；
//   - 未闭合：监听 ready / ctx 取消 / startErr（可为 nil）/ 超时。
//     超时返回 errReadyNotClosed 包装错误，ctx 取消返回 ctx.Err()，
//     Start 失败返回其错误（取出后放回，供收尾等待仍能收到）。
func waitReadyBounded(ctx context.Context, ready <-chan struct{}, timeout time.Duration, startErr chan error) error {
	// 已闭合先于一切判定：Ready 是单调信号，不因关停取消而反悔。
	select {
	case <-ready:
		return peekStartErr(startErr)
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ready:
		return peekStartErr(startErr)
	case err := <-startErr:
		startErr <- err
		// Start 以 nil 返回（服务在等待期间收尾）与取消同时发生时，取消是
		// 权威裁决——返回 nil 会让上层误判就绪。
		if err == nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%w within %v", errReadyNotClosed, timeout)
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
// 顺序语义与既有轮询对齐：先 peek startErr、再查健康（预算边界上恰好转
// 健康仍算成功）、再判预算，最后在等待槽内交错监听 ctx 取消与 startErr。
// 单次调用经 checkHealthBounded 限界：调用上界取剩余预算，预算已耗尽时
// 以 defaultProbeTimeout 兜底"至少检查一次"；挂死的 checker 不能越过
// 预算，ctx 取消优先于超时。startErr 为 nil 时是纯轮询（nil channel
// 永久阻塞，select 安全）。
func awaitHealthy(ctx context.Context, checker Checker, budget, poll time.Duration, startErr chan error, errTimeout func(last error) error) error {
	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var last error
	for {
		if err := peekStartErr(startErr); err != nil {
			return err
		}
		// 取消优先于就绪判定（Start 失败除外）：等待方不陪跑剩余预算。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		bound := time.Until(deadline)
		if bound <= 0 {
			bound = defaultProbeTimeout
		}
		last = checkHealthBounded(ctx, checker, bound)
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
			// Start 以 nil 返回（服务在等待期间收尾）与取消同时发生时，
			// 取消是权威裁决——返回 nil 会让上层误判就绪。
			if err == nil && ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-ticker.C:
		}
	}
}

// checkHealthBounded 以调用方给定的上界执行单次健康检查：经 shutdown.go
// 的 callBounded 原语兜底无 ctx 的 checker。ctx 取消优先于超时：调用方
// ctx 已取消或等待中被取消时返回 ctx.Err()，等待方不陪跑剩余预算；仅
// 上界耗尽时返回超时错误。迟到结果被自然丢弃（goroutine 不因无人接收而
// 阻塞；若 checker 永久挂死，该 goroutine 随之遗留——保证等待循环不挂死
// 优先，与 stopServiceBounded 同一取舍）。
func checkHealthBounded(ctx context.Context, checker Checker, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err, timedOut := callBounded(cctx, checker.CheckHealth)
	if timedOut {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("health check timed out after %v", timeout)
	}
	return err
}
