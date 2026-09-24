package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type memory struct {
	opts coordinatorOptions
	mu   sync.Mutex
	// slots 的键是带 namespace 的完整键。
	slots map[string]*memSlot
	gen   atomic.Uint64
}

type memSlot struct {
	gen    uint64
	exp    time.Time
	renew  bool
	cancel context.CancelFunc
}

// NewMemory 返回进程内 Coordinator，供单测与单进程使用，不能跨进程协调。
func NewMemory(opts ...Option) Coordinator {
	return &memory{
		opts:  applyCoordinatorOptions(opts),
		slots: make(map[string]*memSlot),
	}
}

func (m *memory) Claim(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	if err := ValidateCall(ctx, name, ttl); err != nil {
		return false, err
	}
	now := m.opts.clock().Now()
	key := m.opts.key(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked(now)
	if _, occupied := m.slots[key]; occupied {
		return false, nil
	}
	m.slots[key] = &memSlot{exp: now.Add(ttl)}
	return true, nil
}

func (m *memory) Acquire(ctx context.Context, name string, ttl time.Duration) (Lease, bool, error) {
	if err := ValidateCall(ctx, name, ttl); err != nil {
		return nil, false, err
	}
	now := m.opts.clock().Now()
	key := m.opts.key(name)
	leaseCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.gcLocked(now)
	if _, occupied := m.slots[key]; occupied {
		m.mu.Unlock()
		cancel()
		return nil, false, nil
	}
	gen := m.gen.Add(1)
	m.slots[key] = &memSlot{
		gen:    gen,
		exp:    now.Add(ttl),
		renew:  true,
		cancel: cancel,
	}
	m.mu.Unlock()

	l := &memLease{m: m, key: key, gen: gen, ctx: leaseCtx, cancel: cancel, ttl: ttl}
	go RunRenewLoop(l.ctx, l.cancel, RenewInterval(l.ttl), m.opts.clock(), l.renew)
	return l, true, nil
}

func (m *memory) gcLocked(now time.Time) {
	for k, sl := range m.slots {
		if !sl.renew && !sl.exp.After(now) {
			delete(m.slots, k)
		}
	}
}

type memLease struct {
	m      *memory
	key    string
	gen    uint64
	ttl    time.Duration
	ctx    context.Context
	cancel context.CancelFunc
}

func (l *memLease) Context() context.Context { return l.ctx }

func (l *memLease) Release(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	if sl, ok := l.m.slots[l.key]; ok && sl.gen == l.gen {
		delete(l.m.slots, l.key)
	}
	l.cancel()
	return nil
}

// errLost 标记租约槽已被取代/清除（renew 返回它触发引擎退出）。
var errLost = errors.New("cluster: lease lost")

// renew 刷新槽位过期时间；槽丢失（被覆盖或清除）返回 errLost。
func (l *memLease) renew() error {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	sl, ok := l.m.slots[l.key]
	if !ok || sl.gen != l.gen {
		return errLost
	}
	sl.exp = l.m.opts.clock().Now().Add(l.ttl)
	return nil
}

var _ Coordinator = (*memory)(nil)
var _ Lease = (*memLease)(nil)
