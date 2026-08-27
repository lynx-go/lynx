package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
)

type singleton struct {
	name  string
	inner lynx.Service
	coord Coordinator
	ttl   time.Duration

	stopping atomic.Bool
	leading  atomic.Bool

	mu     sync.Mutex
	cancel context.CancelFunc
}

// Singleton 包装 inner：只有本节点成为 name 的 leader 时才运行 inner.Start。
// 失联后停 inner 并重新竞选。Stop 时 Resign。未当选时 CheckHealth 返回 nil。
func Singleton(name string, inner lynx.Service, s Coordinator) lynx.Service {
	return &singleton{
		name:  name,
		inner: inner,
		coord: s,
		ttl:   DefaultLeaseTTL,
	}
}

func (s *singleton) Name() string {
	if s.inner == nil {
		return s.name
	}
	return s.inner.Name()
}

func (s *singleton) Init(ctx lynx.AppContext) error {
	if s.inner == nil {
		return ErrNilService
	}
	if s.coord == nil {
		return ErrNilCoordinator
	}
	if s.name == "" {
		return ErrEmptyName
	}
	return s.inner.Init(ctx)
}

func (s *singleton) Start(ctx context.Context) error {
	if s.stopping.Load() {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer cancel()

	for {
		if s.stopping.Load() || runCtx.Err() != nil {
			return nil
		}
		lead, err := CampaignTTL(runCtx, s.coord, s.name, s.ttl)
		if err != nil {
			if runCtx.Err() != nil || s.stopping.Load() {
				return nil
			}
			return err
		}
		s.leading.Store(true)
		innerCtx, innerCancel := context.WithCancel(runCtx)
		go func() {
			select {
			case <-lead.Context().Done():
				innerCancel()
			case <-innerCtx.Done():
			}
		}()
		err = s.inner.Start(innerCtx)
		innerCancel()
		s.leading.Store(false)
		_ = lead.Resign(context.Background())
		if s.stopping.Load() || runCtx.Err() != nil {
			return err
		}
	}
}

func (s *singleton) Stop(ctx context.Context) error {
	s.stopping.Store(true)
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.inner == nil {
		return nil
	}
	return s.inner.Stop(ctx)
}

func (s *singleton) CheckHealth() error {
	if !s.leading.Load() {
		return nil
	}
	if c, ok := s.inner.(lynx.Checker); ok {
		return c.CheckHealth()
	}
	return nil
}

var (
	_ lynx.Service = (*singleton)(nil)
	_ lynx.Checker = (*singleton)(nil)
)
