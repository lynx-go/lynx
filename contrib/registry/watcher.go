package registry

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WatcherBase 是推送式 watcher 的共享骨架（泛型化元素类型，供 registry
// 内外的 Discovery 实现复用）：首次 Next 的语义由生产者注入，之后
// 阻塞于推送 / ctx 取消 / Stop 三路；推送为「缓冲 1 最新替换」（慢消费者
// 不排队陈旧快照）；Stop 幂等，并在首次停止时执行注销钩子。
//
// 生产者典型用法：
//
//	type myWatcher struct {
//	    *registry.WatcherBase[registry.Instance]
//	    // 后端引用与索引状态
//	}
//	func (w *myWatcher) Next() ([]registry.Instance, error) {
//	    return w.WatcherBase.Next(w.firstSnapshot)
//	}
type WatcherBase[T any] struct {
	ctx    context.Context
	ch     chan []T
	done   chan struct{}
	once   sync.Once
	first  atomic.Bool
	onStop func()
}

// NewWatcherBase 创建骨架：ctx 取消让阻塞中的 Next 返回 ctx.Err()；
// onStop 在首次 Stop 时执行（可为 nil；不得在 onStop 内再调 Stop）。
func NewWatcherBase[T any](ctx context.Context, onStop func()) *WatcherBase[T] {
	c := &WatcherBase[T]{
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
func (c *WatcherBase[T]) Next(first func() ([]T, error)) ([]T, error) {
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
func (c *WatcherBase[T]) Receive() ([]T, error) {
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
func (c *WatcherBase[T]) stopErr() error {
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
func (c *WatcherBase[T]) Push(snap []T) {
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
func (c *WatcherBase[T]) Drain() {
	select {
	case <-c.ch:
	default:
	}
}

// Stop 停止骨架：幂等；首次调用执行 onStop（注销钩子）并 close(done)。
func (c *WatcherBase[T]) Stop() error {
	c.once.Do(func() {
		if c.onStop != nil {
			c.onStop()
		}
		close(c.done)
	})
	return nil
}

// Done 在 Stop 之后关闭（生产者 loop 的退出信号）。
func (c *WatcherBase[T]) Done() <-chan struct{} { return c.done }

// Ctx 返回创建时的 ctx（生产者 loop 使用）。
func (c *WatcherBase[T]) Ctx() context.Context { return c.ctx }

// SnapshotQuery 取一次后端全量快照（不感知 Filter）：WatcherSession 负责
// 后置 MatchFilter、规范相等与推送。
type SnapshotQuery func(ctx context.Context) ([]Instance, error)

// WatcherSession 是推送式 watcher 的会话核心（组合 WatcherBase）：拥有
// Filter 后置应用、快照规范相等、首快照基线与拉模式退避节奏；后端只提供
// SnapshotQuery 与触发方式（memory 写入时 Submit；DNS/Consul 走 RunPull）。
//
// 不变量：
//   - 快照先按 Filter 过滤、再规范化为唯一形态（canonicalSnapshot）后才
//     进入基线与推送，消费者拿到的一律是规范化快照；
//   - 与已投递快照规范相等的变化不推送（无变化写入 / index 跳变不唤醒）；
//   - 首快照的 Drain 时序由后端 first 闭包负责（memory 需与快照同锁、
//     Consul 需先排空再写 WaitIndex）——那是各后端的原子性约束，不在核心。
type WatcherSession struct {
	*WatcherBase[Instance]

	name   string
	filter Filter

	mu       sync.Mutex
	last     []Instance // 已投递的最新规范快照
	hasState bool       // 是否已建立基线（未建立时首次提交必推送）
}

// NewWatcherSession 创建会话核心。onStop 语义同 NewWatcherBase（首次 Stop
// 时执行注销钩子，可为 nil）。
func NewWatcherSession(ctx context.Context, name string, filter Filter, onStop func()) *WatcherSession {
	return &WatcherSession{
		WatcherBase: NewWatcherBase[Instance](ctx, onStop),
		name:        name,
		filter:      filter,
	}
}

// Next 实现 Watcher.Next：首次调用 first 取原始快照（后端自行保证与 Drain
// 的原子顺序），经后置过滤 + 规范化建立基线；之后阻塞至下一次变化。
func (s *WatcherSession) Next(first SnapshotQuery) ([]Instance, error) {
	return s.WatcherBase.Next(func() ([]Instance, error) {
		raw, err := first(s.Ctx())
		if err != nil {
			return nil, err
		}
		snap := s.normalize(raw)
		s.mu.Lock()
		s.last = snap
		s.hasState = true
		s.mu.Unlock()
		return snap, nil
	})
}

// Submit 提交一次后端全量快照：后置过滤 + 规范化；与已投递快照规范相等
// 则不推送（无变化写入 / 无关 index 跳变不再唤醒消费者）。
//
// 基线建立（首个 Next）前的提交只更新相等基线、不推送：首个 Next 的
// first 查询必返回当前状态，先推只会在「查询与推送竞态」中投递陈旧快照
// （如 Consul 首轮空查询与随后注册的竞态）。
func (s *WatcherSession) Submit(raw []Instance) {
	snap := s.normalize(raw)
	s.mu.Lock()
	if !s.hasState {
		s.last = snap
		s.mu.Unlock()
		return
	}
	if snapshotsEqual(s.last, snap) {
		s.mu.Unlock()
		return
	}
	s.last = snap
	s.mu.Unlock()
	s.Push(snap)
}

func (s *WatcherSession) normalize(raw []Instance) []Instance {
	return canonicalSnapshot(filterInstances(s.filter, raw))
}

// PullPolicy 配置拉模式（轮询 / 长轮询）的节奏与失败退避。
type PullPolicy struct {
	// Interval 是成功后的正常间隔；0 = 长轮询（query 自行阻塞，成功后立即续查）。
	Interval time.Duration
	// MinDelay / MaxDelay 是失败后的退避区间：首次失败取 MinDelay（0 = Interval），
	// 此后逐次倍增、封顶 MaxDelay；成功后复位到 Interval。DNS 的负缓存语义
	// （固定钳制值）用 MinDelay == MaxDelay 表达。
	MinDelay time.Duration
	MaxDelay time.Duration
	// EmptyDelay 是查询成功但快照为空（后端表达「服务不存在」，如 DNS
	// NXDOMAIN）时的下一轮延迟；0 = 与成功一致（Interval）。空快照仍会
	// Submit（服务下线立即生效），只是放慢轮询。
	EmptyDelay time.Duration
}

// nextDelay 计算下一次失败延迟：未达 MinDelay 时取 MinDelay，否则倍增并
// 封顶 MaxDelay。
func (p PullPolicy) nextDelay(prev time.Duration) time.Duration {
	minDelay := p.MinDelay
	if minDelay <= 0 {
		minDelay = p.Interval
	}
	next := prev
	if next < minDelay {
		next = minDelay
	} else {
		next *= 2
	}
	if p.MaxDelay > 0 && next > p.MaxDelay {
		next = p.MaxDelay
	}
	return next
}

// RunPull 启动拉模式循环（阻塞；调用方自行 go）：按 policy 调 query，
// 成功后 Submit 并复位节奏，失败按退避重试；ctx 取消或 Stop 时返回。
// 长轮询（Interval=0）首次立即查询、成功后立即续查；轮询模式首查前先等
// 一个 Interval（首快照已由 Next 的 first 闭包提供）。
func (s *WatcherSession) RunPull(query SnapshotQuery, p PullPolicy) {
	delay := p.Interval
	for {
		if !s.sleep(delay) {
			return
		}
		raw, err := query(s.Ctx())
		if err != nil {
			if s.Ctx().Err() != nil || s.closed() {
				return
			}
			delay = p.nextDelay(delay)
			continue
		}
		delay = p.Interval
		if len(raw) == 0 && p.EmptyDelay > 0 {
			delay = p.EmptyDelay
		}
		s.Submit(raw)
	}
}

// sleep 睡眠 d（d<=0 时只做停止/取消检查）；被 Stop 或 ctx 取消打断返回 false。
func (s *WatcherSession) sleep(d time.Duration) bool {
	if d <= 0 {
		return !s.closed() && s.Ctx().Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.Done():
		return false
	case <-s.Ctx().Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *WatcherSession) closed() bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

// canonicalSnapshot 深拷贝并规范化快照：实例按 ID 排序、Endpoints 按
// (protocol,address) 排序、Tags 排序——规范相等与推送内容共用同一形态，
// 消除后端遍历顺序（memory 的 map 随机序）造成的虚假变化。
func canonicalSnapshot(insts []Instance) []Instance {
	out := make([]Instance, len(insts))
	for i, inst := range insts {
		out[i] = copyInstance(inst)
		slices.SortFunc(out[i].Endpoints, func(a, b Endpoint) int {
			if c := strings.Compare(a.Protocol, b.Protocol); c != 0 {
				return c
			}
			return strings.Compare(a.Address, b.Address)
		})
		slices.Sort(out[i].Tags)
	}
	slices.SortFunc(out, func(a, b Instance) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// snapshotsEqual 是全字段、顺序无关的规范相等判定（输入须为
// canonicalSnapshot 的输出）：实例/Endpoints/Tags 已排序，Meta 用 maps.Equal。
func snapshotsEqual(a, b []Instance) bool {
	return slices.EqualFunc(a, b, func(x, y Instance) bool {
		return x.Name == y.Name && x.ID == y.ID && x.Version == y.Version &&
			x.Status == y.Status && x.Weight == y.Weight &&
			slices.Equal(x.Endpoints, y.Endpoints) &&
			slices.Equal(x.Tags, y.Tags) &&
			maps.Equal(x.Meta, y.Meta)
	})
}
