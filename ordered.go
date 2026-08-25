package lynx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultOrderedReadyTimeout = 10 * time.Second
	orderedHealthPollInterval  = 10 * time.Millisecond
)

// OrderedServices 将多个服务包装成一个 Service。
// Init / Start 按传入顺序执行；Stop 逆序。允许嵌套。
// 子服务不要再单独 Register，否则会重复 Init/Start。
func OrderedServices(name string, services ...Service) Service {
	svcs := make([]Service, len(services))
	copy(svcs, services)
	return &orderedServices{
		name:  name,
		svcs:  svcs,
		ready: make(chan struct{}),
	}
}

type orderedServices struct {
	name         string
	svcs         []Service
	ready        chan struct{}
	readyOnce    sync.Once
	readyTimeout time.Duration // 0 表示使用 defaultOrderedReadyTimeout；仅 Checker 回退路径生效
}

// Ready 在全部子服务就绪后关闭，使嵌套 OrderedServices 能按序等待整组启动完成。
func (g *orderedServices) Ready() <-chan struct{} {
	return g.ready
}

func (g *orderedServices) closeReady() {
	g.readyOnce.Do(func() { close(g.ready) })
}

func (g *orderedServices) Name() string { return g.name }

func (g *orderedServices) timeout() time.Duration {
	if g.readyTimeout > 0 {
		return g.readyTimeout
	}
	return defaultOrderedReadyTimeout
}

func (g *orderedServices) Init(ctx AppContext) error {
	if g.name == "" {
		return errors.New("lynx: OrderedServices name must not be empty")
	}
	if len(g.svcs) == 0 {
		return fmt.Errorf("lynx: OrderedServices %q requires at least one service", g.name)
	}
	for _, s := range g.svcs {
		if s == nil {
			return errors.New("lynx: OrderedServices cannot contain nil service")
		}
	}
	stopCtx := context.Background()
	if ctx != nil {
		stopCtx = ctx.Context()
	}
	for i, s := range g.svcs {
		if err := s.Init(ctx); err != nil {
			// Init 失败时 App 不会登记本包装器，也就不会再调 Stop；
			// 此处是唯一的清理机会，Stop 错误必须一并返回。
			return errors.Join(err, g.stopRange(stopCtx, i))
		}
	}
	return nil
}

func (g *orderedServices) Start(ctx context.Context) error {
	if g.name == "" {
		return errors.New("lynx: OrderedServices name must not be empty")
	}
	if len(g.svcs) == 0 {
		return fmt.Errorf("lynx: OrderedServices %q requires at least one service", g.name)
	}

	type child struct {
		errCh chan error
	}
	children := make([]child, 0, len(g.svcs))

	for _, s := range g.svcs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ch := make(chan error, 1)
		go func(s Service) {
			ch <- s.Start(ctx)
		}(s)
		children = append(children, child{errCh: ch})
		if err := g.waitReady(ctx, s, ch); err != nil {
			return err
		}
	}
	g.closeReady()

	// 全部进入运行后：任一子 Start 返回非 nil 则失败；全部返回则视为收尾完成。
	remaining := len(children)
	merged := make(chan error, remaining)
	for _, c := range children {
		go func(ch chan error) {
			merged <- <-ch
		}(c.errCh)
	}
	for remaining > 0 {
		err := <-merged
		remaining--
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *orderedServices) waitReady(ctx context.Context, s Service, startErr chan error) error {
	if r, ok := s.(Ready); ok {
		return waitReadyChan(ctx, r.Ready(), startErr)
	}
	if c, ok := s.(Checker); ok {
		return g.waitHealthy(ctx, s, c, startErr)
	}
	// 无 Ready / Checker：已 invoke 即继续。若 Start 已立刻失败则上抛（放回供收尾等待）。
	return peekStartErr(startErr)
}

func waitReadyChan(ctx context.Context, ready <-chan struct{}, startErr chan error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-startErr:
		startErr <- err
		return err
	case <-ready:
		// 成功路径：Ready 关闭后 Start 仍阻塞在 Serve。失败路径不得关闭 Ready。
		return peekStartErr(startErr)
	}
}

// peekStartErr 非阻塞读取 Start 结果并放回，供后续收尾等待仍能收到。
func peekStartErr(startErr chan error) error {
	select {
	case err := <-startErr:
		startErr <- err
		return err
	default:
		return nil
	}
}

func (g *orderedServices) waitHealthy(ctx context.Context, s Service, c Checker, startErr chan error) error {
	deadline := time.Now().Add(g.timeout())
	ticker := time.NewTicker(orderedHealthPollInterval)
	defer ticker.Stop()

	var last error
	for {
		if err := peekStartErr(startErr); err != nil {
			return err
		}
		last = c.CheckHealth()
		if last == nil {
			return peekStartErr(startErr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lynx: OrderedServices %q: waiting for %q health timed out after %s: %w",
				g.name, s.Name(), g.timeout(), last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-startErr:
			startErr <- err
			return err
		case <-ticker.C:
		}
	}
}

func (g *orderedServices) Stop(ctx context.Context) error {
	return g.stopRange(ctx, len(g.svcs))
}

// stopRange 逆序停止 svcs[0:n]。
func (g *orderedServices) stopRange(ctx context.Context, n int) error {
	if n > len(g.svcs) {
		n = len(g.svcs)
	}
	var errs []error
	for i := n - 1; i >= 0; i-- {
		if g.svcs[i] == nil {
			continue
		}
		if err := g.svcs[i].Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (g *orderedServices) CheckHealth() error {
	var errs []error
	for _, s := range g.svcs {
		if s == nil {
			continue
		}
		c, ok := s.(Checker)
		if !ok {
			continue
		}
		if err := c.CheckHealth(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
		}
	}
	return errors.Join(errs...)
}

var (
	_ Service = (*orderedServices)(nil)
	_ Checker = (*orderedServices)(nil)
	_ Ready   = (*orderedServices)(nil)
)
