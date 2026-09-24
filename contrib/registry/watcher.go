package registry

import (
	"context"
	"sync"
	"sync/atomic"
)

// WatcherCore[T] 是推送式 watcher 的共享骨架（泛型化元素类型，供 registry
// 内外的 Discovery 实现复用）：首次 Next 的语义由生产者注入，之后阻塞于
// 推送 / ctx 取消 / Stop 三路；推送为「缓冲 1 最新替换」（慢消费者不排队
// 陈旧快照）；Stop 幂等，并在首次停止时执行注销钩子。
//
// 生产者典型用法：
//
//	type myWatcher struct {
//	    *registry.WatcherCore[registry.Instance]
//	    // 后端引用与索引状态
//	}
//	func (w *myWatcher) Next() ([]registry.Instance, error) {
//	    return w.WatcherCore.Next(w.firstSnapshot)
//	}
type WatcherCore[T any] struct {
	ctx    context.Context
	ch     chan []T
	done   chan struct{}
	once   sync.Once
	first  atomic.Bool
	onStop func()
}

// NewWatcherCore 创建骨架：ctx 取消让阻塞中的 Next 返回 ctx.Err()；
// onStop 在首次 Stop 时执行（可为 nil；不得在 onStop 内再调 Stop）。
func NewWatcherCore[T any](ctx context.Context, onStop func()) *WatcherCore[T] {
	c := &WatcherCore[T]{
		ctx:    ctx,
		ch:     make(chan []T, 1),
		done:   make(chan struct{}),
		onStop: onStop,
	}
	c.first.Store(true)
	return c
}

// Next 实现 Watcher.Next：首次调用执行 first（生产者在此完成「取快照 +
// 排空积压通知」的原子序列；first 为 nil 时直接进入等待，依赖预推的首
// 快照），之后阻塞至下一次 Push、ctx 取消或 Stop。停止/取消是权威裁决：
// 先于挂起推送返回（Stop 之后不交付任何快照）。
func (c *WatcherCore[T]) Next(first func() ([]T, error)) ([]T, error) {
	if c.first.CompareAndSwap(true, false) && first != nil {
		if err := c.stopErr(); err != nil {
			return nil, err
		}
		snap, err := first()
		if err != nil {
			return nil, err
		}
		// first 执行期间发生停止：同样以停止为准（丢弃快照）。
		if err := c.stopErr(); err != nil {
			return nil, err
		}
		return snap, nil
	}
	return c.Receive()
}

// Receive 阻塞等待下一次推送 / ctx 取消 / Stop（首次快照逻辑内部需要
// 等待时复用）。停止/取消优先于挂起推送。
func (c *WatcherCore[T]) Receive() ([]T, error) {
	if err := c.stopErr(); err != nil {
		return nil, err
	}
	select {
	case snap := <-c.ch:
		// 与停止/取消交错：丢弃快照，以停止/取消结果为准。
		if err := c.stopErr(); err != nil {
			return nil, err
		}
		return snap, nil
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	case <-c.done:
		return nil, ErrWatcherStopped
	}
}

// stopErr 返回停止（done 已关闭，优先）或 ctx 取消错误；均未发生返回 nil。
func (c *WatcherCore[T]) stopErr() error {
	select {
	case <-c.done:
		return ErrWatcherStopped
	default:
	}
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
	}
	return nil
}

// Push 推送最新快照：缓冲 1，满时以新值替换旧值（合并陈旧推送）。
func (c *WatcherCore[T]) Push(snap []T) {
	select {
	case c.ch <- snap:
		return
	default:
	}
	// 排空旧值后重试；两次 select 的交错只会丢弃更旧的值，不会阻塞。
	select {
	case <-c.ch:
	default:
	}
	select {
	case c.ch <- snap:
	default:
	}
}

// Drain 排空尚未消费的推送（生产者首次快照去重时使用）。
func (c *WatcherCore[T]) Drain() {
	select {
	case <-c.ch:
	default:
	}
}

// Stop 停止骨架：幂等；首次调用执行 onStop（注销钩子）并 close(done)。
func (c *WatcherCore[T]) Stop() error {
	c.once.Do(func() {
		if c.onStop != nil {
			c.onStop()
		}
		close(c.done)
	})
	return nil
}

// Done 在 Stop 之后关闭（生产者 loop 的退出信号）。
func (c *WatcherCore[T]) Done() <-chan struct{} { return c.done }

// Ctx 返回创建时的 ctx（生产者 loop 使用）。
func (c *WatcherCore[T]) Ctx() context.Context { return c.ctx }
