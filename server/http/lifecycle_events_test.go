package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
)

// busContext 是仅提供 Bus 的最小 AppContext（HTTP Init 只消费 Bus；其余
// 方法不会被本用例触达）。
type busContext struct {
	lynx.AppContext
	bus eventbus.Bus
}

func (c busContext) Bus() eventbus.Bus { return c.bus }

// TestServerLifecycleEvents：HTTP 生命周期事件经共享主题发出
// （Service=http）：listening → stopping → stopped，与 debug 侧同一组
// lynx.server.* 主题。
func TestServerLifecycleEvents(t *testing.T) {
	bus := eventbus.NewMemoryBus(eventbus.Options{})
	subCtx := eventbus.ContextWithBus(context.Background(), bus)
	listening := make(chan eventbus.ServerEvent, 1)
	stopping := make(chan eventbus.ServerEvent, 1)
	stopped := make(chan eventbus.ServerEvent, 1)
	subscribe := func(topic eventbus.Topic[eventbus.ServerEvent], ch chan eventbus.ServerEvent) {
		t.Helper()
		if err := topic.Subscribe(subCtx, func(_ context.Context, e *eventbus.Event[eventbus.ServerEvent]) error {
			ch <- e.Payload
			return nil
		}); err != nil {
			t.Fatalf("Subscribe %s: %v", topic.Name(), err)
		}
	}
	subscribe(eventbus.ServerListeningTopic, listening)
	subscribe(eventbus.ServerStoppingTopic, stopping)
	subscribe(eventbus.ServerStoppedTopic, stopped)

	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), WithAddr("127.0.0.1:0"), WithAdvertiseAddr("adv.example:80"))
	if err := srv.Init(busContext{bus: bus}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("server did not become ready")
	}

	expect := func(what string, ch chan eventbus.ServerEvent, addr, adv string) {
		t.Helper()
		select {
		case e := <-ch:
			if e.Service != "http" || e.Addr != addr || e.AdvertiseAddr != adv {
				t.Fatalf("%s event = %+v, want service=http addr=%q adv=%q", what, e, addr, adv)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s event not received", what)
		}
	}
	expect("listening", listening, srv.Addr(), "adv.example:80")

	stopAddr := srv.Addr()
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expect("stopping", stopping, stopAddr, "adv.example:80")
	expect("stopped", stopped, "", "")
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}
