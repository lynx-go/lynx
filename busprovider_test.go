package lynx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// publishRecordingBus 在 readyGateBus 之上记录 Publish 的 topic，用于验证
// provider 构建的总线真正承接了应用侧发布。
type publishRecordingBus struct {
	readyGateBus
	mu        sync.Mutex
	published []string
}

func (b *publishRecordingBus) Publish(ctx context.Context, topic string, payload any, opts ...eventbus.PublishOption) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, topic)
	return nil
}

func (b *publishRecordingBus) topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published...)
}

// providerSvc 是 provider 返回的配套服务假件：记录生命周期调用，Start
// 阻塞到 ctx 取消（与真实 Transport 一致的 actor 形态），实现 Checker。
type providerSvc struct {
	inited  bool
	stopped bool
}

func (s *providerSvc) Name() string { return "provider-svc" }
func (s *providerSvc) Init(ctx AppContext) error {
	s.inited = true
	return nil
}
func (s *providerSvc) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (s *providerSvc) Stop(ctx context.Context) error {
	s.stopped = true
	return nil
}
func (s *providerSvc) CheckHealth() error { return nil }

// TestWithBusProviderResolvesAfterConfig：provider 在配置装配后被调用
// （拿到非 nil Config 与键值），返回的总线成为 app 总线，配套服务 Init
// 并进入健康聚合。
func TestWithBusProviderResolvesAfterConfig(t *testing.T) {
	v := viper.New()
	v.Set("k", "v")
	bus := &publishRecordingBus{}
	svc := &providerSvc{}
	var gotCfg Config
	app, err := newLynx(NewOptions(
		WithConfig(NewViperConfig(v)),
		WithIsolated(),
		WithBusProvider(func(cfg Config) (eventbus.Bus, []Service, error) {
			gotCfg = cfg
			return bus, []Service{svc}, nil
		}),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	defer app.Close()
	if gotCfg == nil {
		t.Fatal("provider was not called (cfg is nil)")
	}
	if gotCfg.GetString("k") != "v" {
		t.Fatalf("provider cfg k = %q, want v", gotCfg.GetString("k"))
	}
	if app.Bus() != bus {
		t.Fatal("app.Bus() should be the provider-built bus")
	}
	if !svc.inited {
		t.Fatal("provider service should be Init'd at construction")
	}
	found := false
	for _, c := range app.HealthCheckers() {
		if c == Checker(svc) {
			found = true
		}
	}
	if !found {
		t.Fatal("provider service (Checker) should be in HealthCheckers()")
	}
}

// TestWithBusProviderPrecedenceWithBus：显式 WithBus 优先，provider 不被咨询。
func TestWithBusProviderPrecedenceWithBus(t *testing.T) {
	explicit := &readyGateBus{}
	called := false
	app, err := newLynx(NewOptions(
		WithBus(explicit),
		WithIsolated(),
		WithBusProvider(func(cfg Config) (eventbus.Bus, []Service, error) {
			called = true
			return &readyGateBus{}, nil, nil
		}),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	defer app.Close()
	if called {
		t.Fatal("WithBusProvider should not be consulted when WithBus is set")
	}
	if app.Bus() != explicit {
		t.Fatal("app.Bus() should be the explicitly injected bus")
	}
}

// TestWithBusProviderWithBusOptionsOrderIndependent：WithBusOptions 不得
// 静默击败 provider——无论两个 Option 的先后顺序（先应用时它会物化内存
// 总线到 o.Bus，provider 仍须覆盖物化结果；后应用时直接跳过）。
func TestWithBusProviderWithBusOptionsOrderIndependent(t *testing.T) {
	orders := map[string]func(bus *readyGateBus) []Option{
		"busOptions first": func(b *readyGateBus) []Option {
			return []Option{WithBusOptions(), WithBusProvider(providerOf(b))}
		},
		"provider first": func(b *readyGateBus) []Option {
			return []Option{WithBusProvider(providerOf(b)), WithBusOptions()}
		},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			bus := &readyGateBus{}
			opts := append([]Option{WithIsolated()}, order(bus)...)
			app, err := newLynx(NewOptions(opts...))
			if err != nil {
				t.Fatalf("newLynx() error = %v", err)
			}
			defer app.Close()
			if app.Bus() != eventbus.Bus(bus) {
				t.Fatal("app.Bus() should be the provider-built bus")
			}
		})
	}
}

// TestWithBusProviderStillLosesToExplicitBus：WithBusOptions 物化后再被
// 显式 WithBus 覆盖时，provider 让位于显式实例（WithBus 清除物化标记）。
func TestWithBusProviderStillLosesToExplicitBus(t *testing.T) {
	explicit := &readyGateBus{}
	app, err := newLynx(NewOptions(
		WithIsolated(),
		WithBusOptions(),
		WithBus(explicit),
		WithBusProvider(providerOf(&readyGateBus{})),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	defer app.Close()
	if app.Bus() != explicit {
		t.Fatal("explicit WithBus must win over both WithBusOptions and provider")
	}
}

func providerOf(b eventbus.Bus) func(Config) (eventbus.Bus, []Service, error) {
	return func(Config) (eventbus.Bus, []Service, error) { return b, nil, nil }
}

// TestWithBusProviderError：provider 错误经构造错误路径返回并带上下文。
func TestWithBusProviderError(t *testing.T) {
	_, err := newLynx(NewOptions(
		WithIsolated(),
		WithBusProvider(func(cfg Config) (eventbus.Bus, []Service, error) {
			return nil, nil, errors.New("boom")
		}),
	))
	if err == nil || !strings.Contains(err.Error(), "bus provider failed") {
		t.Fatalf("newLynx() error = %v, want wrapped bus provider failure", err)
	}
}

// TestWithBusProviderNilBus：nil 总线视为构造错误（否则后续总线链路空指针）。
func TestWithBusProviderNilBus(t *testing.T) {
	_, err := newLynx(NewOptions(
		WithIsolated(),
		WithBusProvider(func(cfg Config) (eventbus.Bus, []Service, error) {
			return nil, nil, nil
		}),
	))
	if err == nil || !strings.Contains(err.Error(), "nil bus") {
		t.Fatalf("newLynx() error = %v, want nil bus rejection", err)
	}
}

// TestWithBusProviderSlowBusReadiness：provider 总线走既有 BusReadyTimeout
// 就绪等待——慢启动总线在预算内构造成功。
func TestWithBusProviderSlowBusReadiness(t *testing.T) {
	bus := &readyGateBus{readyAfter: 100 * time.Millisecond}
	start := time.Now()
	app, err := newLynx(NewOptions(
		WithIsolated(),
		WithBusProvider(providerOf(bus)),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v, want success within default BusReadyTimeout", err)
	}
	defer app.Close()
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("newLynx() returned after %v, want to wait for slow bus readiness", elapsed)
	}
}

// TestWithBusProviderLifecycleInRun：端到端——命令经 app ctx 发布命中
// provider 总线（ctx 内嵌的是最终总线），配套服务在关停时被 Stop。
func TestWithBusProviderLifecycleInRun(t *testing.T) {
	bus := &publishRecordingBus{}
	svc := &providerSvc{}
	r := NewRunner(func(app App) error {
		return app.Command(func(ctx context.Context) error {
			if got := eventbus.BusFromContext(ctx); got != eventbus.Bus(bus) {
				return errors.New("ctx should carry the provider-built bus")
			}
			return bus.Publish(ctx, "cli.done", nil)
		})
	},
		WithIsolated(),
		WithBusProvider(func(cfg Config) (eventbus.Bus, []Service, error) {
			return bus, []Service{svc}, nil
		}),
	)
	if err := r.RunE(); err != nil {
		t.Fatalf("RunE() error = %v", err)
	}
	topics := bus.topics()
	found := false
	for _, tp := range topics {
		if tp == "cli.done" {
			found = true
		}
	}
	if !found {
		t.Errorf("bus topics = %v, want cli.done published on provider bus", topics)
	}
	if !svc.stopped {
		t.Fatal("provider service should be stopped after command completes")
	}
}
