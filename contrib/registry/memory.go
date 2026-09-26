package registry

import (
	"context"
	"errors"
	"maps"
	"sync"
)

var (
	// errClosed 在 Memory.Close 之后的写操作或 Watch 上返回。
	errClosed = errors.New("registry: memory backend closed")
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
	w := &memoryWatcher{m: m, name: name}
	w.session = NewWatcherSession(ctx, name, filter, func() {
		m.mu.Lock()
		if set, ok := m.watchers[name]; ok {
			delete(set, w)
			if len(set) == 0 {
				delete(m.watchers, name)
			}
		}
		m.mu.Unlock()
	})
	set, ok := m.watchers[name]
	if !ok {
		set = make(map[*memoryWatcher]struct{})
		m.watchers[name] = set
	}
	set[w] = struct{}{}
	return w, nil
}

// notifyLocked 向该服务的全部 Watcher 提交全量快照（过滤 + 规范相等 +
// 推送由 WatcherSession 统一完成；无变化写入不再唤醒消费者）。
// 调用方必须持有 m.mu（写锁）。
func (m *Memory) notifyLocked(name string) {
	for w := range m.watchers[name] {
		w.session.Submit(m.rawSnapshotLocked(w.name))
	}
}

// snapshotLocked 返回过滤后的深拷贝快照（读路径）。调用方必须持有 m.mu。
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

// rawSnapshotLocked 返回未过滤的深拷贝快照（WatcherSession 提交入口：
// 过滤统一在会话核心后置应用）。调用方必须持有 m.mu。
func (m *Memory) rawSnapshotLocked(name string) []Instance {
	set := m.services[name]
	out := make([]Instance, 0, len(set))
	for _, inst := range set {
		out = append(out, copyInstance(inst))
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

// memoryWatcher 是 Memory.Watch 返回的 Watcher：触发为写入侧推送
// （notifyLocked → session.Submit），会话核心负责过滤/相等/推送。
type memoryWatcher struct {
	m       *Memory
	name    string
	session *WatcherSession
}

// Next 首次调用立即返回当前快照（含空列表）；之后阻塞至集合变化、
// ctx 取消或 Stop。
func (w *memoryWatcher) Next() ([]Instance, error) {
	return w.session.Next(w.firstSnapshot)
}

// firstSnapshot 在锁内取全量快照并排空积压通知：先于首次 Next 发生的变化
// 已包含在当前快照中，不应再重复推送；过滤由会话核心后置应用。
func (w *memoryWatcher) firstSnapshot(context.Context) ([]Instance, error) {
	w.m.mu.RLock()
	defer w.m.mu.RUnlock()
	snap := w.m.rawSnapshotLocked(w.name)
	w.session.Drain()
	return snap, nil
}

// Stop 停止 Watcher 并从 Memory 注销；幂等，返回 nil。
func (w *memoryWatcher) Stop() error {
	return w.session.Stop()
}
