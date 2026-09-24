package registry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrResolverClosed 在 Resolver.Close 之后的 Get/GetAll 上返回。
var ErrResolverClosed = errors.New("registry: resolver closed")

const (
	// watchBackoffMin / watchBackoffMax 是 Watch 断开后指数退避重连的上下限。
	watchBackoffMin = 1 * time.Second
	watchBackoffMax = 30 * time.Second
)

// filterAll 用于向 Discovery 拉取/订阅全量实例（含非 Passing）。
// 缓存键只用服务名，调用方 Filter 在读路径由 MatchFilter 应用，
// 这样同进程内不同 Filter 共享一条缓存与一个 watch goroutine。
var filterAll = Filter{IncludeUnhealthy: true}

// ResolverOption 配置 Resolver。
type ResolverOption func(*Resolver)

// WithPicker 替换默认的 RoundRobinPicker。
func WithPicker(p Picker) ResolverOption {
	return func(r *Resolver) {
		if p != nil {
			r.picker = p
		}
	}
}

// WithStaleMaxAge 设置缓存快照的最大陈旧年龄，默认 60s（= 2 × 默认
// heartbeat_ttl，见设计文档「缓存规则」）。Watch 断开期间可继续提供
// 最后一次成功快照（stale-while-revalidate），年龄超过该上限即丢弃，
// Get 返回 ErrNoInstance，禁止分区后无限供应死实例。
func WithStaleMaxAge(d time.Duration) ResolverOption {
	return func(r *Resolver) {
		if d > 0 {
			r.staleMaxAge = d
		}
	}
}

// WithPollInterval 设置 Watch 不可用时的轮询间隔，默认 15s。
// Watch 建立失败后按该间隔 GetService，并在下个周期重试 Watch。
func WithPollInterval(d time.Duration) ResolverOption {
	return func(r *Resolver) {
		if d > 0 {
			r.pollInterval = d
		}
	}
}

// WithResolverLogger 注入 Resolver 内部日志（stale 丢弃 warn、watch 重连
// debug）。此前走全局 slog.Warn，与包内其余组件的注入式 logger 不一致，
// 进程内多 Resolver（测试场景）无法分离日志（RC-20）。缺省回退
// slog.Default()，保持既有行为。
func WithResolverLogger(l *slog.Logger) ResolverOption {
	return func(r *Resolver) {
		if l != nil {
			r.logger = l
		}
	}
}

// Resolver 是带进程内缓存的客户端发现：每个服务名一条缓存 +
// 一个后台 watch goroutine（Watch 失败时回退轮询）。
// 并发安全；Close 幂等。
type Resolver struct {
	d            Discovery
	picker       Picker
	staleMaxAge  time.Duration
	pollInterval time.Duration
	logger       *slog.Logger

	mu      sync.Mutex
	entries map[string]*cacheEntry // key = 服务名，见 filterAll 注释
	closed  bool

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewResolver 返回基于 d 的 Resolver。默认 RoundRobinPicker，
// Filter 零值（只 Passing），stale 上限 60s（2 × 默认 heartbeat_ttl）。
func NewResolver(d Discovery, opts ...ResolverOption) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Resolver{
		d:            d,
		picker:       RoundRobinPicker(),
		staleMaxAge:  60 * time.Second,
		pollInterval: 15 * time.Second,
		logger:       slog.Default(),
		entries:      make(map[string]*cacheEntry),
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Get 按 filter 过滤缓存后由 Picker 选一条实例。空服务名返回 ErrBadName；
// 过滤后为空返回 ErrNoInstance。缓存未填充时同步 GetService 一次。
func (r *Resolver) Get(ctx context.Context, name string, filter Filter) (Instance, error) {
	insts, err := r.lookup(ctx, name, filter)
	if err != nil {
		return Instance{}, err
	}
	return r.picker.Pick(insts) // Pick 空切片返回 ErrNoInstance
}

// GetAll 返回按 filter 过滤后的全部缓存实例。后端空切片视为
// 「当前无实例」，返回 ([], nil)，不引入 ErrNotFound。
func (r *Resolver) GetAll(ctx context.Context, name string, filter Filter) ([]Instance, error) {
	return r.lookup(ctx, name, filter)
}

// Close 停掉全部 watch goroutine 并拒绝后续读取；幂等，返回 nil。
func (r *Resolver) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	close(r.done)
	r.cancel() // 解除阻塞中的 Watcher.Next
	r.wg.Wait()
	return nil
}

// lookup 是 Get/GetAll 的公共读路径。
func (r *Resolver) lookup(ctx context.Context, name string, filter Filter) ([]Instance, error) {
	if name == "" {
		return nil, ErrBadName
	}
	e, ok := r.entryFor(name)
	if !ok {
		return nil, ErrResolverClosed
	}
	if err := r.ensureFilled(ctx, name, e); err != nil {
		return nil, err
	}
	insts, err := r.snapshot(name, e)
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(insts))
	for _, inst := range insts {
		if MatchFilter(filter, inst) {
			out = append(out, copyInstance(inst))
		}
	}
	return out, nil
}

// entryFor 取该服务名的缓存条目，不存在时创建并启动 watch goroutine。
func (r *Resolver) entryFor(name string) (*cacheEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	e, ok := r.entries[name]
	if !ok {
		e = &cacheEntry{}
		r.entries[name] = e
		r.wg.Add(1)
		go r.watchLoop(name, e)
	}
	return e, true
}

// ensureFilled 在缓存未填充时同步 GetService 一次（首个 Get 不等待
// watch 首轮推送）。并发首个 Get 可能重复拉取，结果等价，不加锁串行化。
//
// 已知权衡（RC-22，接受）：本方法与 watchLoop 并发写同一 cacheEntry，
// 交错时缓存可能短暂回退到稍旧的快照（store 非原子读-改-写）。下一次
// watch 推送即收敛；不引入额外同步的原因是读路径无锁快照的性能收益
// 与该窗口（毫秒级、自愈）不成正比。
func (r *Resolver) ensureFilled(ctx context.Context, name string, e *cacheEntry) error {
	e.mu.RLock()
	filled := e.filled
	e.mu.RUnlock()
	if filled {
		return nil
	}
	insts, err := r.d.GetService(ctx, name, filterAll)
	if err != nil {
		return err
	}
	e.store(insts)
	return nil
}

// snapshot 返回未做调用方 Filter 的缓存快照。快照年龄超过 staleMaxAge
// 时丢弃（打 warn，只打一次）并返回 ErrNoInstance。
func (r *Resolver) snapshot(name string, e *cacheEntry) ([]Instance, error) {
	e.mu.RLock()
	insts, filled, updatedAt := e.instances, e.filled, e.updatedAt
	e.mu.RUnlock()
	if !filled {
		return nil, ErrNoInstance
	}
	if time.Since(updatedAt) <= r.staleMaxAge {
		return insts, nil
	}
	e.mu.Lock()
	if e.instances != nil && time.Since(e.updatedAt) > r.staleMaxAge {
		r.logger.Warn("registry: resolver dropped stale snapshot",
			"service", name,
			"age", time.Since(e.updatedAt).Round(time.Millisecond),
			"stale_max_age", r.staleMaxAge)
		e.instances = nil
	}
	e.mu.Unlock()
	return nil, ErrNoInstance
}

// watchLoop 维护单个服务名的缓存：首选 Watch 推送；Next 出错按
// 1s–30s 指数退避重连；Watch 建立失败回退按 pollInterval 轮询
// GetService 并在下个周期重试 Watch。
func (r *Resolver) watchLoop(name string, e *cacheEntry) {
	defer r.wg.Done()
	backoff := watchBackoffMin
	for {
		select {
		case <-r.done:
			return
		default:
		}
		w, err := r.d.Watch(r.ctx, name, filterAll)
		if err != nil {
			// Watch 不可用（如 DNS 后端）：本轮直接轮询一次。
			if insts, gerr := r.d.GetService(r.ctx, name, filterAll); gerr == nil {
				e.store(insts)
			}
			if !r.sleep(r.pollInterval) {
				return
			}
			continue
		}
		closed, received := r.consume(name, w, e)
		// 无论因何退出会话都 Stop：closed 路径（Resolver 关闭）此前直接
		// return 不调 Stop，后端 watcher 条目永久残留，此后的 Register/
		// Deregister 持续向死 watcher 推送（内存泄漏 + 无效工作，RC-04）。
		_ = w.Stop()
		if closed {
			return
		}
		// Watch 断开：退避重连，期间读路径继续提供最后一次成功快照，
		// 直至年龄超过 staleMaxAge 被丢弃。收到过快照的会话视为健康
		// 连接，重置退避；从未收到快照的反复断开则按 1s–30s 增长。
		if received {
			backoff = watchBackoffMin
		}
		if !r.sleep(backoff) {
			return
		}
		backoff = min(backoff*2, watchBackoffMax)
	}
}

// consume 消费 Watcher 推送直至出错或 Resolver 关闭。
// 空快照同样立即生效（服务下线），不回退旧非空列表。
// closed=true 表示 Resolver 已关闭；received 报告本会话是否收到过快照。
func (r *Resolver) consume(name string, w Watcher, e *cacheEntry) (closed, received bool) {
	for {
		snap, err := w.Next()
		if err != nil {
			select {
			case <-r.done:
				return true, received
			default:
			}
			if errors.Is(err, context.Canceled) {
				return true, received
			}
			r.logger.Debug("registry: resolver watch error, reconnecting",
				"service", name, "error", err)
			return false, received
		}
		e.store(snap)
		received = true
	}
}

// sleep 睡眠 d，期间 Resolver 关闭则返回 false。
func (r *Resolver) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.done:
		return false
	case <-t.C:
		return true
	}
}

// cacheEntry 是单个服务名的缓存条目：最近一次成功快照及其时间，
// 外加消费侧订阅（Subscribe）表。
type cacheEntry struct {
	mu        sync.RWMutex
	instances []Instance // 全量快照（含非 Passing），Filter 在订阅/读路径应用
	updatedAt time.Time
	filled    bool
	// subs 是订阅者表：store 对每个订阅应用其 Filter 后 Push（缓冲 1
	// 最新替换）——慢订阅者总是拿到最新快照，不排队陈旧快照。
	// nextSubID 自增分配、不复用。
	subs      map[uint64]*subscription
	nextSubID uint64
}

// store 写入新快照。空切片也是合法快照（服务下线），立即生效。
// store 是缓存变更的唯一入口（watchLoop 推送、轮询回退、ensureFilled
// 同步首填全部经此），因此也是订阅通知的唯一触发点。
func (e *cacheEntry) store(insts []Instance) {
	e.mu.Lock()
	e.instances = insts
	e.filled = true
	e.updatedAt = time.Now()
	for _, sub := range e.subs {
		sub.push(insts)
	}
	e.mu.Unlock()
}

// addSub 注册订阅者；缓存已填充时预推一份当前过滤快照（Subscribe 契约：
// 首个 Next 立即返回当前快照；未填充时首个 Next 等待首次 store）。
func (e *cacheEntry) addSub(r *Resolver, filter Filter) *subscription {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.subs == nil {
		e.subs = make(map[uint64]*subscription)
	}
	e.nextSubID++
	sub := &subscription{r: r, e: e, id: e.nextSubID, filter: filter}
	sub.core = NewWatcherCore[Instance](r.ctx, nil)
	e.subs[sub.id] = sub
	if e.filled {
		sub.push(e.instances)
	}
	return sub
}

// removeSub 注销订阅者（Stop 路径），此后 store 不再向其发信号。
func (e *cacheEntry) removeSub(id uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.subs, id)
}

// Subscribe 订阅服务实例集变化（缓存层订阅）：后端 Watch 推送/轮询
// 兜底已由 Resolver 内部维护，订阅者对后端形态无感（DNS 后端下缓存由
// 轮询维护，同样触发通知）。契约与 Discovery.Watcher 同构：
//
//   - 缓存已填充时首个 Next 立即返回当前快照；未填充则等待首次 store；
//   - 后续 Next 返回最近一次快照变化；慢消费者不排队陈旧快照（推送
//     缓冲 1 最新替换，总是最新）；空切片同样是合法推送（服务下线立即
//     生效）；
//   - 快照已按 filter 过滤（MatchFilter 语义），与 Watch/GetAll 读路径
//     一致；每个服务名共享一条缓存与推送源，过滤在订阅边界应用；
//   - 返回切片只读（不深拷贝，Instance 内部切片与缓存共享）；
//   - Stop 退订且幂等，之后 Next 返回 ErrWatcherStopped；
//     Resolver.Close 后 Next 返回 ErrResolverClosed。
//
// stale 丢弃（快照超龄）不触发通知：与 gRPC resolver"解析出错保留
// 上次状态"的惯例一致，由消费方自行处理。
func (r *Resolver) Subscribe(name string, filter Filter) (Watcher, error) {
	if name == "" {
		return nil, ErrBadName
	}
	e, ok := r.entryFor(name)
	if !ok {
		return nil, ErrResolverClosed
	}
	return e.addSub(r, filter), nil
}

// subscription 是 Subscribe 返回的缓存订阅，实现 Watcher。
type subscription struct {
	r      *Resolver
	e      *cacheEntry
	id     uint64
	filter Filter
	core   *WatcherCore[Instance]
}

// Next 阻塞至缓存变化并返回已过滤的最新快照（语义见 Subscribe）。
func (s *subscription) Next() ([]Instance, error) {
	insts, err := s.core.Next(nil)
	if err != nil && errors.Is(err, context.Canceled) {
		// 订阅没有自己的 ctx：取消只可能来自 Resolver.Close。
		return nil, ErrResolverClosed
	}
	return insts, err
}

// push 应用订阅 Filter 后推送（store 持缓存锁调用；Push 非阻塞）。
func (s *subscription) push(insts []Instance) {
	s.core.Push(filterInstances(s.filter, insts))
}

// Stop 注销订阅并唤醒阻塞中的 Next；幂等，返回 nil。
func (s *subscription) Stop() error {
	// core.Stop 幂等；removeSub 对已删除项是 no-op → 整体幂等。
	err := s.core.Stop()
	s.e.removeSub(s.id)
	return err
}

// EndpointOf 在已选 Instance 上按协议取地址：稳定顺序（切片下标）下
// 第一条 Protocol 匹配的 Endpoint。protocol 空则取 Endpoints[0]。
// 无匹配返回 ErrNoInstance。
func EndpointOf(inst Instance, protocol string) (Endpoint, error) {
	if protocol == "" {
		if len(inst.Endpoints) == 0 {
			return Endpoint{}, ErrNoInstance
		}
		return inst.Endpoints[0], nil
	}
	for _, ep := range inst.Endpoints {
		if ep.Protocol == protocol {
			return ep, nil
		}
	}
	return Endpoint{}, ErrNoInstance
}
