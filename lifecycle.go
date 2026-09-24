package lynx

// lifecycle 的唯一归属：actor 调度、关停阶段序列与注册状态机。
// Run / Close / command.Stop 是本模块的薄入口。
//
// 阶段顺序是不变量，不依赖 actor 注册顺序：任一触发（actor 返回、退出
// 信号、Close、OnPostStart 钩子错误）都进入同一序列——drain 置位 +
// OnDrain 窗口 → cancelCtx → OnPreStop → 逆序有界停止服务（LIFO）→
// AppStopped → 有界停总线 → 等全部 actor 退出 → 一次性聚合错误返回。
// 关停阶段只在服务已进入运行阶段后执行；Init / OnPreStart 失败路径由
// Run 走「逆序有界停止已 Init 的服务 + OnPostStop」的快路径。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// actor 是 lifecycle 调度的执行单元：execute 进入运行并阻塞至自行返回
// 或被 stop 停止；stop 请求停止（有界、幂等；停止错误经 stopServiceBounded
// 记入 shutdownErrors，由 runLifecycle 统一聚合）。
type actor struct {
	name    string
	execute func() error
	stop    func()
}

// actorResult 是 execute 的返回记录：name 供日志与触发定位。
type actorResult struct {
	name string
	err  error
}

// runLifecycle 驱动一次应用运行：并发启动全部 actor，等待首个触发，
// 随后按文件头的阶段序列关停，并一次性聚合全部错误返回。
func (app *lynx) runLifecycle(exitCh <-chan os.Signal) error {
	app.mu.Lock()
	actors := append([]actor(nil), app.actors...)
	app.mu.Unlock()

	done := make(chan actorResult, len(actors))
	for _, a := range actors {
		a := a
		go func() { done <- actorResult{name: a.name, err: a.execute()} }()
	}

	// OnPostStart：所有 actor 进入执行体（startWG 归零）后执行；错误进
	// 触发通道。关停触发即取消 postStartCtx：阻塞中的钩子视为被打断，
	// 不再等待（其 goroutine 仍在后台运行）。
	postStartCtx, postStartCancel := context.WithCancel(context.WithoutCancel(app.ctx))
	defer postStartCancel()
	postStartDone := make(chan error, 1)
	go func() {
		app.startWG.Wait()
		if err := app.runOnPostStartHooks(postStartCtx); err != nil {
			postStartDone <- err
		}
	}()

	// 等待首个触发：钩子正常完成不触发（继续等待其它触发）。
	var (
		runErr    error
		trigger   string
		received  int
		triggered bool
	)
	for !triggered {
		select {
		case r := <-done:
			received++
			runErr, trigger, triggered = r.err, "service:"+r.name, true
		case err := <-postStartDone:
			runErr, trigger, triggered = err, "on-post-start", true
		case <-app.ctx.Done():
			runErr, trigger, triggered = nil, "app-context", true
		case <-exitCh:
			runErr, trigger, triggered = nil, "signal", true
		}
	}
	app.Logger().Info("shutting down", "trigger", trigger)
	app.publishAppEvent(eventbus.TopicAppStopping)
	postStartCancel()

	// 阶段序列（顺序即契约，见文件头）。
	drainErr := app.runDrainPhase()
	app.cancelCtx()
	shutdownErr := app.runOnPreStopHooks()
	app.stopActors(actors)
	for received < len(actors) {
		<-done
		received++
	}
	app.publishAppEvent(eventbus.TopicAppStopped)
	app.stopBusBounded()
	var shutdownErrs error
	if app.shutdownErrors.HasErrors() {
		shutdownErrs = &app.shutdownErrors
	}
	return errors.Join(runErr, drainErr, shutdownErr, shutdownErrs)
}

// stopActors 逆序有界停止全部服务（LIFO：后注册的先停，与 OrderedServices
// 组内顺序一致）。停止错误由 stopServiceBounded 记入 shutdownErrors。
func (app *lynx) stopActors(actors []actor) {
	for i := len(actors) - 1; i >= 0; i-- {
		actors[i].stop()
	}
}

// failStart 收尾启动期早退路径：发布 AppStopped 后有界停止提前 Start 的
// 总线。Run 在入口已置位 running，Close 不再兜底总线；post-stop 钩子由
// Run 的 defer 覆盖。
func (app *lynx) failStart() {
	app.publishAppEvent(eventbus.TopicAppStopped)
	app.stopBusBounded()
}

// stopBusBounded 有界停止应用总线：在 AppStopped 事件之后、Run 返回之前，
// 保证收尾事件仍可投递。错误经 stopServiceBounded 记入 shutdownErrors。
func (app *lynx) stopBusBounded() {
	if app.bus == nil {
		return
	}
	app.publishEvent(eventbus.TopicServiceStopping, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
	app.stopServiceBounded(context.WithoutCancel(app.ctx), busService{app.bus})
	app.publishEvent(eventbus.TopicServiceStopped, eventbus.ServiceEvent{Service: app.bus.Name(), Time: time.Now()})
	if app.busCancel != nil {
		app.busCancel()
	}
}

// serviceActor 构造服务的 actor：execute 是服务执行体（startWG 计数、
// 中断先行检查、生命周期事件与 Start）；stop 是停止体（事件、有界 Stop、
// 取消服务 ctx）。两层守卫（execute 侧中断检查 + 服务自身 Stop-wins 标志）
// 覆盖「Stop 先于 Start 执行」的交错。
func (app *lynx) serviceActor(ctx context.Context, cancel context.CancelFunc, service Service) actor {
	return actor{
		name: service.Name(),
		execute: func() error {
			// 进入执行体即计数归零：post-start 以"所有服务 actor 已进入
			// 执行体"为触发界（阻塞型服务的 Start 关停前不返回）。
			app.startWG.Done()
			select {
			case <-ctx.Done():
				app.logger.InfoContext(ctx, "service start skipped, interrupted before start",
					"service", service.Name())
				return nil
			default:
			}
			app.logger.InfoContext(ctx, "starting service", "service", service.Name())
			app.publishEvent(eventbus.TopicServiceStarting, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			// Started 事件契约：语义是"已进入运行"，不是"Start 已成功返回"。
			// 发布点固定在 Start 调用之前——阻塞式服务（如 HTTP/gRPC server）
			// 的 Start 只在关停时才返回，若等返回后再发布，订阅者整个生命周期
			// 都收不到 Started。代价：快速返回型服务若 Start 立即失败，订阅者
			// 会看到 Starting→Started→Failed 的时序，Failed 才是权威裁决。
			app.publishEvent(eventbus.TopicServiceStarted, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			err := service.Start(ctx)
			if err != nil {
				app.publishEvent(eventbus.TopicServiceFailed, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now(), Error: err.Error()})
			}
			return err
		},
		stop: func() {
			app.logger.InfoContext(ctx, "stopping service", "service", service.Name())
			app.publishEvent(eventbus.TopicServiceStopping, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			app.stopServiceBounded(ctx, service)
			// 统一发布 Stopped：Stop 错误无法精确归属到单个服务（stopServiceBounded
			// 聚合进 shutdownErrors 由 Run 上抛），订阅侧以 Run 返回值/日志为准。
			app.publishEvent(eventbus.TopicServiceStopped, eventbus.ServiceEvent{Service: service.Name(), Time: time.Now()})
			cancel()
		},
	}
}

// checkRegistration 是注册协议的唯一入口（调用方在服务 Init 之前调用）：
//   - (nil, nil)：放行；
//   - (initErr, nil)：注册阶段的 poison-pill——首个注册错误已记录，
//     Register/RegisterFactory 静默返回，Command 返回该错误；
//   - (nil, err)：errRunStarted（运行中）或 ErrAppClosed（已关闭），
//     由调用方经 registrationError 翻译为 panic / 错误。
func (app *lynx) checkRegistration() (initErr, err error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.running {
		return nil, errRunStarted
	}
	if app.closed {
		return nil, ErrAppClosed
	}
	return app.initErr, nil
}

// registrationError 把注册裁决错误翻译为对外的明确错误（panic 值或返回值）。
func registrationError(method string, err error) error {
	switch {
	case errors.Is(err, errRunStarted):
		return fmt.Errorf("lynx: %s must not be called after Run() has started", method)
	case errors.Is(err, ErrAppClosed):
		return fmt.Errorf("lynx: %s must not be called after Close(): %w", method, ErrAppClosed)
	default:
		return err
	}
}

// registerService 在持锁登记事务内登记一个已 Init 成功的服务与其 actor：
// 与 Run 的 running 置位互斥，迟到的注册在此被裁决（errRunStarted /
// ErrAppClosed），不产生「已 Init、永不 Start/Stop」的孤儿。
func (app *lynx) registerService(service Service, a actor) error {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.running {
		return errRunStarted
	}
	if app.closed {
		return ErrAppClosed
	}
	app.actors = append(app.actors, a)
	app.services = append(app.services, service)
	app.startWG.Add(1)
	if hc, ok := service.(Checker); ok {
		app.healthCheckers = append(app.healthCheckers, hc)
	}
	return nil
}
