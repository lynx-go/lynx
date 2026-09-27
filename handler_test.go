package lynx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/lynx-go/lynx/lynxtest"
)

// orderCreated 是测试用域事件。
type orderCreated struct{ ID string }

var orderCreatedTopic = eventbus.NewTopic[orderCreated]("test.order.created")

// testHandler 是带依赖注入的典型 handler：name 构造期注入，injected
// 由 Init 注入（模拟从 AppContext 取配置）。
type testHandler struct {
	name     string
	injected bool
	initErr  error
	got      chan *eventbus.Event[orderCreated]
}

func (h *testHandler) Topic() eventbus.Topic[orderCreated] { return orderCreatedTopic }
func (h *testHandler) HandlerName() string                 { return h.name }

func (h *testHandler) Init(lynx.AppContext) error {
	if h.initErr != nil {
		return h.initErr
	}
	h.injected = true
	return nil
}

func (h *testHandler) Handle(_ context.Context, e *eventbus.Event[orderCreated]) error {
	if !h.injected {
		// 适配器保证 Init 先于订阅；走到这里说明顺序契约被破坏。
		return errors.New("handle called before Init injection")
	}
	if h.got != nil {
		select {
		case h.got <- e:
		default:
		}
	}
	return nil
}

func noopHandler(context.Context, *eventbus.Event[orderCreated]) error { return nil }

var _ lynx.Service = (*lynx.EventHandlerService[orderCreated])(nil)

// TestEventHandlerServiceNameBeforeInit：Name 不依赖 Init（框架可能在 Init 前
// 调用），nil 服务返回空串而非 panic。
func TestEventHandlerServiceNameBeforeInit(t *testing.T) {
	svc := lynx.NewEventHandlerService(&testHandler{name: "order-created"})
	if got := svc.Name(); got != "order-created" {
		t.Fatalf("Name() = %q, want %q", got, "order-created")
	}
	if got := (*lynx.EventHandlerService[orderCreated])(nil).Name(); got != "" {
		t.Fatalf("nil Name() = %q, want empty", got)
	}
}

// TestEventHandlerServiceDeliversAfterInit：Init 先注入依赖再订阅，事件经
// 解码后到达 Handle。
func TestEventHandlerServiceDeliversAfterInit(t *testing.T) {
	actx := lynxtest.NewContext(t)
	h := &testHandler{name: "order-created", got: make(chan *eventbus.Event[orderCreated], 1)}
	svc := lynx.NewEventHandlerService(h)
	if err := svc.Init(actx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := orderCreatedTopic.Publish(actx.Context(), orderCreated{ID: "o-1"}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case e := <-h.got:
		if e.Payload.ID != "o-1" {
			t.Fatalf("payload ID = %q, want %q", e.Payload.ID, "o-1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive event")
	}
}

// TestEventHandlerServiceInitErrorSkipsSubscribe：h.Init 失败即返回、不订阅
// （同名 handler 仍可直接订阅，证明名字未被占用）。
func TestEventHandlerServiceInitErrorSkipsSubscribe(t *testing.T) {
	actx := lynxtest.NewContext(t)
	boom := errors.New("boom")
	h := &testHandler{name: "failed-handler", initErr: boom}
	if err := lynx.NewEventHandlerService(h).Init(actx); !errors.Is(err, boom) {
		t.Fatalf("Init() error = %v, want %v", err, boom)
	}
	if err := orderCreatedTopic.Subscribe(actx.Context(), noopHandler,
		eventbus.WithHandlerName("failed-handler")); err != nil {
		t.Fatalf("failed Init must not subscribe, got: %v", err)
	}
}

// TestEventHandlerServiceHandlerNameDefaultAndOverride：默认 handler 名 =
// HandlerName；显式 WithHandlerName 覆盖默认。
func TestEventHandlerServiceHandlerNameDefaultAndOverride(t *testing.T) {
	actx := lynxtest.NewContext(t)
	if err := lynx.NewEventHandlerService(&testHandler{name: "default-name"}).Init(actx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := orderCreatedTopic.Subscribe(actx.Context(), noopHandler,
		eventbus.WithHandlerName("default-name")); err == nil {
		t.Fatal("default handler name not applied: duplicate subscribe succeeded")
	}

	if err := lynx.NewEventHandlerService(&testHandler{name: "svc-name"},
		eventbus.WithHandlerName("override-name")).Init(actx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := orderCreatedTopic.Subscribe(actx.Context(), noopHandler,
		eventbus.WithHandlerName("override-name")); err == nil {
		t.Fatal("WithHandlerName override not applied: duplicate subscribe succeeded")
	}
}

// TestEventHandlerServiceInitRejectsNilAndEmptyName：防御路径返回明确错误。
func TestEventHandlerServiceInitRejectsNilAndEmptyName(t *testing.T) {
	actx := lynxtest.NewContext(t)
	if err := (*lynx.EventHandlerService[orderCreated])(nil).Init(actx); err == nil {
		t.Fatal("nil service Init() should fail")
	}
	if err := lynx.NewEventHandlerService(&testHandler{}).Init(actx); err == nil {
		t.Fatal("empty handler name Init() should fail")
	}
}

// TestEventHandlerServiceStartBlocksUntilShutdown：Start 阻塞至 ctx 取消；
// Stop 无操作返回 nil。
func TestEventHandlerServiceStartBlocksUntilShutdown(t *testing.T) {
	svc := lynx.NewEventHandlerService(&testHandler{name: "blocking"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Start(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Start returned before shutdown: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
}
