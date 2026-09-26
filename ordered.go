package lynx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const defaultOrderedReadyTimeout = 10 * time.Second

// OrderedOption 配置 OrderedServices 组行为。
type OrderedOption func(*orderedServices)

// WithOrderedReadyTimeout 覆盖每子服务的就绪等待预算（默认
// defaultOrderedReadyTimeout = 10s；嵌套组逐层按各自子服务分别计时）。
// 非正值使用默认值。
func WithOrderedReadyTimeout(d time.Duration) OrderedOption {
	return func(g *orderedServices) { g.readyTimeout = d }
}

// NewOrderedServices 是 OrderedServices 的选项化构造入口：启停语义与
// OrderedServices 完全一致，额外接受组级选项（如 WithOrderedReadyTimeout）。
func NewOrderedServices(name string, services []Service, opts ...OrderedOption) Service {
	svcs := make([]Service, len(services))
	copy(svcs, services)
	g := &orderedServices{
		name:  name,
		svcs:  svcs,
		ready: make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

// OrderedServices 将多个服务包装成一个 Service。
// Init / Start 按传入顺序执行；Stop 逆序。允许嵌套。
// 子服务不要再单独 Register，否则会重复 Init/Start。
// 每个子服务的 Init / Start / Stop 都会记录 Info 日志（service=子服务名、
// group=组名），组内启动停滞时据此定位到具体子服务。
// 每子服务就绪预算默认 10s；需要调整用 NewOrderedServices +
// WithOrderedReadyTimeout。
func OrderedServices(name string, services ...Service) Service {
	return NewOrderedServices(name, services)
}

type orderedServices struct {
	name         string
	svcs         []Service
	ready        chan struct{}
	readyOnce    sync.Once
	readyTimeout time.Duration // 0 表示使用 defaultOrderedReadyTimeout；Ready 与 Checker 两条路径共用
	logger       *slog.Logger  // Init 捕获（Start/Stop 没有 AppContext）；nil 时回退 slog.Default()
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

// logChild 记录一条子服务生命周期日志：消息与应用级 serviceActor 一致，
// service 属性为子服务名；group 属性由 Init 捕获的 logger 预置。未经 Init
// （或 Init 收到 nil AppContext）时回退默认 logger，缺 logger 不丢日志。
func (g *orderedServices) logChild(ctx context.Context, msg string, s Service) {
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	logger.InfoContext(ctx, msg, "service", s.Name())
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
		// Start/Stop 只收到 context.Context，组 logger 只能在此捕获。
		g.logger = ctx.Logger("group", g.name)
	}
	for i, s := range g.svcs {
		g.logChild(stopCtx, "initializing service", s)
		if err := s.Init(ctx); err != nil {
			// Init 失败时 App 不会登记本包装器，也就不会再调 Stop；
			// 此处是唯一的清理机会，Stop 错误必须一并返回。
			return errors.Join(err, g.stopRange(stopCtx, i))
		}
		g.logChild(stopCtx, "initialized service", s)
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
		g.logChild(ctx, "starting service", s)
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
	return resolveProbe(s).wait(ctx, g.timeout(), startErr, func(last error) error {
		return fmt.Errorf("lynx: OrderedServices %q: waiting for %q health timed out after %s: %w",
			g.name, s.Name(), g.timeout(), last)
	})
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
		s := g.svcs[i]
		if s == nil {
			continue
		}
		g.logChild(ctx, "stopping service", s)
		if err := s.Stop(ctx); err != nil {
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
