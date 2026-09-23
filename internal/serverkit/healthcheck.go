// Package serverkit 是 HTTP / gRPC / debug 三个 server 适配器共享规则的
// 唯一归属：健康检查执行、有界优雅关停、请求标识传播与生命周期事件。
// 根级 internal：对 server/* 与 debug 同时可见，不进入公开 API。
package serverkit

import (
	"fmt"
	"time"

	"github.com/lynx-go/lynx"
)

// DefaultHealthCheckTimeout 是单个 checker 的执行上限（SC-03）：
// CheckHealth 接口无 ctx 参数，挂死的 checker 只能被"放弃等待"。
const DefaultHealthCheckTimeout = 3 * time.Second

// RunHealthChecks 并发执行 checkers 并整体限时 timeout：任一失败立即返回
// 其错误；超时返回超时错误（视为不健康）。lynx.Checker 接口无 ctx 参数
// （API 冻结），挂死的 checker 无法被打断，只能被"放弃等待"——结果
// channel 带缓冲，最终返回的 checker goroutine 写入后自行退出，不阻塞
// 探测方。固有边界（放弃等待模式的残余代价）：永不返回的 checker（死锁
// 类）其 goroutine 无法回收，会随每次探测累积泄漏，只能修复 checker
// 本身或重启进程消除。
// timeout <= 0 表示不限时：退化为顺序执行（保持旧行为的逃生口）。
func RunHealthChecks(checkers lynx.HealthCheckersFunc, timeout time.Duration) error {
	cs := checkers()
	if len(cs) == 0 {
		return nil
	}
	if timeout <= 0 {
		for _, c := range cs {
			if err := checkOne(c); err != nil {
				return err
			}
		}
		return nil
	}
	results := make(chan error, len(cs))
	for _, c := range cs {
		go func(c lynx.Checker) {
			results <- checkOne(c)
		}(c)
	}
	// checker 并发起步，共享一个计时窗口即等效"每个 checker 限时"。
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for i := 0; i < len(cs); i++ {
		select {
		case err := <-results:
			if err != nil {
				return err
			}
		case <-timer.C:
			return fmt.Errorf("health check timed out after %s", timeout)
		}
	}
	return nil
}

// checkOne 执行单个 checker 并兜底其 panic：并发路径下 checker 运行在
// 独立 goroutine，panic 无外层中间件可恢复（Recovery 只覆盖 handler
// goroutine），必须就地 recover 并按不健康处理，避免拖垮进程。
func checkOne(c lynx.Checker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("health check panicked: %v", r)
		}
	}()
	return c.CheckHealth()
}
