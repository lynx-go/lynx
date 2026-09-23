package lynx

// 关停阶段执行器与有界原语的唯一归属：排水检查器与窗口、各阶段钩子
// 执行器、有界停止与有界调用原语。阶段时序（谁先谁后）在 lifecycle.go；
// 预算不变量（DrainTimeout + ShutdownTimeout + Σ StopTimeout +
// CleanupTimeout）见 lifecycle.go 文件头与 options.go 各预算字段注释。

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// ErrDraining 是排水窗口内 readiness 聚合返回的错误。排水置位后
// drainChecker.CheckHealth 返回该错误，调用方可用 errors.Is 匹配
// （例如 contrib 模块在排水边沿执行从服务目录注销）。
var ErrDraining = errors.New("draining")

// drainChecker 是框架内部的排水检查器（不导出）：关停流程进入排水窗口时
// 置位 draining，使 readiness 聚合（app.HealthCheckers()）立即失败，
// 让负载均衡器在真实关停前完成摘流。仅当 Options.DrainTimeout > 0 时由
// newLynx 注册进 healthCheckers；DrainTimeout=0（默认）时不注册，
// HealthCheckers() 快照内容与 v1.0 完全一致。
type drainChecker struct {
	draining atomic.Bool
}

// SetDraining 设置排水状态。
func (d *drainChecker) SetDraining(draining bool) {
	d.draining.Store(draining)
}

// CheckHealth 实现 Checker：排水期间返回 ErrDraining，其余返回 nil。
func (d *drainChecker) CheckHealth() error {
	if d.draining.Load() {
		return ErrDraining
	}
	return nil
}

var _ Checker = (*drainChecker)(nil)

// callBounded 在 ctx 预算内等待 fn 返回：fn 在独立 goroutine 执行，超时或
// 取消时 timedOut=true 并返回 ctx.Err()；fn 正常返回时透传其错误。
// fn 所在 goroutine 不被终止、迟到结果经缓冲 chan 自然丢弃（挂死的 fn
// 遗留 goroutine——保证等待方不挂死优先，全模块同一取舍）。timedOut 是
// 结构性判定（哪一侧 channel 就绪），不用错误值区分——fn 自身也可能返回
// context 错误，那属于 fn 的错误而非预算耗尽。
func callBounded(ctx context.Context, fn func() error) (err error, timedOut bool) {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err = <-done:
		return err, false
	case <-ctx.Done():
		return ctx.Err(), true
	}
}

// stopServiceBounded 有界停止单个服务：超过 StopTimeout 后记录错误并继续，
// 防止挂死的服务 Stop 阻塞整个关停流程。
// 注意：超时后服务 Stop 仍在后台 goroutine 运行，若其永久阻塞则该 goroutine
// 随之泄漏（可接受的取舍——保证关停流程不被挂死优先）。
// 预算独立于入参 ctx：关停路径中 app.ctx 可能已取消（shutdown Step 1），
// 上界只由 StopTimeout 决定，与既有 time.After 语义一致。
// 服务 Stop 返回的错误与超时错误写入 shutdownErrors，由 Run() 统一上抛，
// 使调用方（如 K8s）能感知服务级关停失败。
func (app *lynx) stopServiceBounded(ctx context.Context, service Service) {
	stopCtx, cancel := context.WithTimeout(context.Background(), app.o.StopTimeout)
	defer cancel()
	stopErr, timedOut := callBounded(stopCtx, func() error { return service.Stop(ctx) })
	if timedOut {
		stopErr = fmt.Errorf("service %q stop timed out after %v", service.Name(), app.o.StopTimeout)
		app.logger.ErrorContext(app.ctx, "service stop timed out",
			"service", service.Name(), "timeout", app.o.StopTimeout.String())
	}
	if stopErr != nil {
		app.logger.ErrorContext(app.ctx, "service stop error",
			"service", service.Name(), "error", stopErr)
		app.shutdownErrors.Add(stopErr)
	}
}

// stopServices 逆序停止已注册服务，用于 Init/OnPreStart 失败路径的资源清理。
// 正常关停路径由 lifecycle 的 stopActors 逆序停止（LIFO），与本函数一致。
func (app *lynx) stopServices(ctx context.Context) {
	app.mu.Lock()
	svcs := append([]Service(nil), app.services...)
	app.mu.Unlock()
	for i := len(svcs) - 1; i >= 0; i-- {
		app.stopServiceBounded(ctx, svcs[i])
	}
}

// hasDrainHooks 报告是否注册了 OnDrain 钩子。无钩子时关停路径整段跳过
// 钩子执行，不增加任何等待（回归红线：默认关停上界与既有版本一致）。
func (app *lynx) hasDrainHooks() bool {
	app.mu.Lock()
	defer app.mu.Unlock()
	return len(app.onDrains) > 0
}

// runDrainPhase 执行排水窗口：置位 drainChecker（readiness 聚合立即失败，
// LB 摘流），窗口睡眠与 OnDrain 钩子并发执行——窗口即钩子总预算，上界不
// 叠加（DrainTimeout=0 时整段跳过，Run 入口已保证此路径不可能有已注册的
// OnDrain 钩子）。返回钩子错误（无钩子或窗口未启用时 nil）。
func (app *lynx) runDrainPhase() error {
	if app.drain != nil {
		app.drain.SetDraining(true)
	}
	if app.o.DrainTimeout <= 0 {
		return nil
	}
	app.publishEvent(eventbus.TopicDrainStarting, eventbus.DrainEvent{Timeout: app.o.DrainTimeout, Time: time.Now()})
	var drainErr error
	drainHooksDone := make(chan struct{})
	if app.hasDrainHooks() {
		// 窗口 deadline 即钩子总预算：在窗口开启时创建并传入，钩子预算
		// 与窗口严格对齐（无论钩子 goroutine 调度迟早）。
		drainCtx, drainCancel := context.WithTimeout(context.Background(), app.o.DrainTimeout)
		defer drainCancel()
		go func() {
			defer close(drainHooksDone)
			drainErr = app.runOnDrainHooks(drainCtx)
		}()
	} else {
		close(drainHooksDone)
	}
	app.Logger().Info("draining: readiness marked unhealthy, waiting for drain window",
		"drain_timeout", app.o.DrainTimeout.String())
	// 窗口不可被 ctx 取消打断：排水语义要求服务在窗口内保持运行，
	// 供在途请求收尾。
	time.Sleep(app.o.DrainTimeout)
	app.publishEvent(eventbus.TopicDrainCompleted, eventbus.DrainEvent{Timeout: app.o.DrainTimeout, Time: time.Now()})
	// 等待 OnDrain 钩子收尾（受窗口预算约束，不会挂死）。
	<-drainHooksDone
	return drainErr
}

// shutdownPhase 描述一个带预算的关停钩子阶段，参数化两处逐行重复的
// 钩子执行器（runOnDrainHooks / runOnPreStopHooks）在日志与错误文案上的
// 全部差异。
type shutdownPhase struct {
	hook    string // 钩子名："on-drain" / "on-pre-stop"
	budget  string // 预算名："drain hook" / "shutdown"
	summary string // 汇总日志前缀："drain hooks" / "shutdown"
}

var (
	drainHookPhase   = shutdownPhase{hook: "on-drain", budget: "drain hook", summary: "drain hooks"}
	preStopHookPhase = shutdownPhase{hook: "on-pre-stop", budget: "shutdown", summary: "shutdown"}
)

// runHooksInBudget 在 ctx 预算内顺序执行关停钩子（deadline 由调用方创建：
// OnDrain 为排水窗口、OnPreStop 为 ShutdownTimeout）。单个钩子阻塞不会
// 挂起关停流程：超时记错误并继续；钩子错误不打断执行。错误聚合为
// *ShutdownErrors 返回（无错误时 nil），由 Run() 统一上抛给调用方。
func (app *lynx) runHooksInBudget(ctx context.Context, p shutdownPhase, hooks []HookFunc) error {
	var errs ShutdownErrors
	for _, fn := range hooks {
		if ctx.Err() != nil {
			errs.Add(fmt.Errorf("%s timeout exceeded while running %s hooks", p.budget, p.hook))
			break
		}
		hookErr, timedOut := callBounded(ctx, func() error { return fn(ctx) })
		switch {
		case timedOut:
			app.logger.ErrorContext(app.ctx, p.hook+" hook did not complete within "+p.budget+" timeout")
			errs.Add(fmt.Errorf("%s hook timed out", p.hook))
		case hookErr != nil:
			app.logger.ErrorContext(app.ctx, p.hook+" hook called error", "error", hookErr)
			errs.Add(hookErr)
		}
	}
	if errs.HasErrors() {
		app.logger.ErrorContext(app.ctx, p.summary+" completed with errors", "errors", errs.Error())
		return &errs
	}
	return nil
}

// runOnDrainHooks 在排水窗口预算内顺序执行所有 OnDrain hooks（ctx 由
// shutdown 闭包在窗口开启时创建，deadline 即窗口结束）。语义对齐
// runOnPreStopHooks：单个 hook 阻塞不会挂起整个关闭流程，超时记录错误
// 并继续；钩子错误不打断排水。不传没有 deadline 的 app.ctx。
func (app *lynx) runOnDrainHooks(ctx context.Context) error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onDrains...)
	app.mu.Unlock()

	app.Logger().Info("run on-drain hooks")
	return app.runHooksInBudget(ctx, drainHookPhase, hooks)
}

// runOnPreStopHooks 在 ShutdownTimeout 内顺序执行所有 OnPreStop hooks。
// 单个 hook 阻塞不会挂起整个关闭流程：超过时限后记录错误并继续。
// 收集到的错误（含超时）以 *ShutdownErrors 返回，由 Run() 上抛给调用方。
func (app *lynx) runOnPreStopHooks() error {
	app.mu.Lock()
	hooks := append([]HookFunc(nil), app.onPreStops...)
	app.mu.Unlock()

	app.Logger().Info("run on-pre-stop hooks")
	ctx, cancel := context.WithTimeout(context.Background(), app.o.ShutdownTimeout)
	defer cancel()
	return app.runHooksInBudget(ctx, preStopHookPhase, hooks)
}

// runPostStopHooks 逆序执行 OnPostStop hooks：进程收尾清理（关闭
// DB/Redis 连接池等）。取走即清空——Close() 的兜底路径与 Run 的 defer
// 路径不会重复执行。总预算 CleanupTimeout：单个钩子挂起时记日志跳过
// 剩余钩子（其 goroutine 仍在后台运行，与 stopServiceBounded 相同的
// 取舍——不阻塞进程退出优先）。钩子签名为 CleanupFunc（无错误返回）：
// 终局阶段错误没有消费者，实现方自行记日志。
func (app *lynx) runPostStopHooks() {
	app.mu.Lock()
	fns := app.onPostStops
	app.onPostStops = nil
	app.mu.Unlock()
	if len(fns) == 0 {
		return
	}
	app.Logger().Info("run on-post-stop hooks")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), app.o.CleanupTimeout)
	defer cancel()
loop:
	for i := len(fns) - 1; i >= 0; i-- {
		_, timedOut := callBounded(ctx, func() error { fns[i](); return nil })
		if timedOut {
			// 倒序执行：fns[i] 超时，尚未执行的 fns[0..i-1] 共 i 个被跳过。
			app.logger.ErrorContext(app.ctx, "on-post-stop hook did not complete within cleanup timeout",
				"timeout", app.o.CleanupTimeout.String(), "skipped", i)
			break loop
		}
	}
	app.Logger().Info("on-post-stop hooks finished", "elapsed", time.Since(start).Round(time.Millisecond).String())
}
