package registry

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
)

var (
	// errClosed 在 Memory.Close 之后的写操作或 Watch 上返回。
	errClosed = errors.New("registry: memory backend closed")
	// errWatcherStopped 在 Watcher.Stop 之后阻塞中的 Next 上返回。
	errWatcherStopped = errors.New("registry: watcher stopped")
)

// Memory 是进程内 Registry + Discovery，用于测试与单进程场景。
// 原生存储 Instance（含多 Endpoint），不经任何 Meta/JSON 编码。
// 并发安全；Deregister / Close 幂等。
type Memory struct {
	mu       sync.RWMutex
	services map[string]map[string]Instance // name -> id -> instance
	watchers map[string]map[*memoryWatcher]struct{}
	closed   bool
}

var (
	_ Registry  = (*Memory)(nil)
	_ Discovery = (*Memory)(nil)
)

// NewMemory 返回一个空的进程内后端。
func NewMemory() *Memory {
	return &Memory{
		services: make(map[string]map[string]Instance),
		watchers: make(map[string]map[*memoryWatcher]struct{}),
	}
}

// Register 按 ID upsert（last-write-wins），随后向 Watchers 推送新快照。
func (m *Memory) Register(_ context.Context, inst Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errClosed
	}
	set, ok := m.services[inst.Name]
	if !ok {
		set = make(map[string]Instance)
		m.services[inst.Name] = set
	}
	set[inst.ID] = copyInstance(inst)
	m.notifyLocked(inst.Name)
	return nil
}

// Deregister 删除实例；不存在时为 no-op（幂等）。删除成功时推送新快照。
func (m *Memory) Deregister(_ context.Context, serviceName, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errClosed
	}
	set, ok := m.services[serviceName]
	if !ok {
		return nil
	}
	if _, existed := set[instanceID]; !existed {
		return nil
	}
	delete(set, instanceID)
	if len(set) == 0 {
		delete(m.services, serviceName)
	}
	m.notifyLocked(serviceName)
	return nil
}

// Heartbeat 是 no-op：memory 后端没有 TTL。
func (m *Memory) Heartbeat(_ context.Context, _, _ string) error { return nil }

// Close 停止全部 Watcher 并拒绝后续写入；幂等。
func (m *Memory) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	watchers := make([]*memoryWatcher, 0)
	for _, set := range m.watchers {
		for w := range set {
			watchers = append(watchers, w)
		}
	}
	m.mu.Unlock()
	for _, w := range watchers {
		_ = w.Stop()
	}
	return nil
}

// GetService 返回应用 Filter 后的深拷贝快照；无实例时返回空切片与 nil。
func (m *Memory) GetService(_ context.Context, name string, filter Filter) ([]Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotLocked(name, filter), nil
}

// Watch 返回一个 Watcher：首次 Next 立即推送当前快照（含空列表），
// 之后集合每次变化推送一次新快照。
func (m *Memory) Watch(ctx context.Context, name string, filter Filter) (Watcher, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errClosed
	}
	w := &memoryWatcher{
		m:      m,
		name:   name,
		filter: filter,
		ctx:    ctx,
		ch:     make(chan []Instance, 1),
		done:   make(chan struct{}),
	}
	w.first.Store(true)
	set, ok := m.watchers[name]
	if !ok {
		set = make(map[*memoryWatcher]struct{})
		m.watchers[name] = set
	}
	set[w] = struct{}{}
	return w, nil
}

// notifyLocked 向该服务的全部 Watcher 推送各自的过滤后快照。
// 调用方必须持有 m.mu（写锁）。
func (m *Memory) notifyLocked(name string) {
	for w := range m.watchers[name] {
		snap := m.snapshotLocked(w.name, w.filter)
		// 缓冲 1 + 最新替换：Watcher 不消费时只保留最新快照。
		select {
		case w.ch <- snap:
		default:
			select {
			case <-w.ch:
			default:
			}
			w.ch <- snap
		}
	}
}

// snapshotLocked 返回过滤后的深拷贝快照。调用方必须持有 m.mu。
func (m *Memory) snapshotLocked(name string, filter Filter) []Instance {
	set := m.services[name]
	out := make([]Instance, 0, len(set))
	for _, inst := range set {
		if MatchFilter(filter, inst) {
			out = append(out, copyInstance(inst))
		}
	}
	return out
}

// copyInstance 深拷贝 Endpoints / Tags / Meta，保证快照不被调用方篡改。
func copyInstance(in Instance) Instance {
	out := in
	out.Endpoints = append([]Endpoint(nil), in.Endpoints...)
	out.Tags = append([]string(nil), in.Tags...)
	if in.Meta != nil {
		out.Meta = maps.Clone(in.Meta)
	}
	return out
}

// memoryWatcher 是 Memory.Watch 返回的 Watcher。
type memoryWatcher struct {
	m      *Memory
	name   string
	filter Filter
	ctx    context.Context
	ch     chan []Instance // 缓冲 1，最新替换
	done   chan struct{}
	once   sync.Once
	first  atomic.Bool
}

// Next 首次调用立即返回当前快照（含空列表）；之后阻塞至集合变化、
// ctx 取消或 Stop。
func (w *memoryWatcher) Next() ([]Instance, error) {
	if w.first.CompareAndSwap(true, false) {
		// 在 RLock 内取快照并排空积压通知：先于首次 Next 发生的变化
		// 已包含在当前快照中，不应再重复推送。
		w.m.mu.RLock()
		snap := w.m.snapshotLocked(w.name, w.filter)
		select {
		case <-w.ch:
		default:
		}
		w.m.mu.RUnlock()
		return snap, nil
	}
	select {
	case snap := <-w.ch:
		return snap, nil
	case <-w.ctx.Done():
		return nil, w.ctx.Err()
	case <-w.done:
		return nil, errWatcherStopped
	}
}

// Stop 停止 Watcher 并从 Memory 注销；幂等，返回 nil。
func (w *memoryWatcher) Stop() error {
	w.once.Do(func() {
		close(w.done)
		w.m.mu.Lock()
		if set, ok := w.m.watchers[w.name]; ok {
			delete(set, w)
			if len(set) == 0 {
				delete(w.m.watchers, w.name)
			}
		}
		w.m.mu.Unlock()
	})
	return nil
}
