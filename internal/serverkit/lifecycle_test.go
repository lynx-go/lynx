package serverkit

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// TestLifecycleStartGuard 钉住重入守卫与复位语义：二次 BeginStart 报错
// （带 service 名）；AbortStart（启动失败）允许重试；ResetForInit 复位
// 守卫与停止标志。
func TestLifecycleStartGuard(t *testing.T) {
	l := NewLifecycle("http")
	if err := l.BeginStart(); err != nil {
		t.Fatalf("first BeginStart = %v, want nil", err)
	}
	err := l.BeginStart()
	if err == nil || !strings.Contains(err.Error(), "http server: Start called more than once") {
		t.Fatalf("second BeginStart = %v, want reentry guard error", err)
	}

	// Listen/端点配置失败：AbortStart 复位，允许重试。
	l.AbortStart()
	if err := l.BeginStart(); err != nil {
		t.Fatalf("BeginStart after AbortStart = %v, want nil", err)
	}

	// Init：ResetForInit 复位守卫与停止标志（ready 不可复位，语义见注释）。
	l.BeginStop()
	l.ResetForInit()
	if err := l.BeginStart(); err != nil {
		t.Fatalf("BeginStart after ResetForInit = %v, want nil", err)
	}
	if l.StopRequested() {
		t.Fatal("StopRequested after ResetForInit = true, want false")
	}
}

// TestLifecycleReadyOnce 钉住就绪信号的一次性语义：MarkReady 前不关闭，
// 重复 MarkReady 不 panic。
func TestLifecycleReadyOnce(t *testing.T) {
	l := NewLifecycle("grpc")
	select {
	case <-l.Ready():
		t.Fatal("Ready closed before MarkReady")
	default:
	}
	l.MarkReady()
	l.MarkReady()
	select {
	case <-l.Ready():
	case <-time.After(time.Second):
		t.Fatal("Ready not closed after MarkReady")
	}
}

// TestLifecycleEvents 钉住事件发布：Service 名固定为构造值，Addr/
// AdvertiseAddr 按参数进入事件，三个主题各收到一次（bus 为 nil 时 no-op）。
func TestLifecycleEvents(t *testing.T) {
	bus := eventbus.NewMemoryBus(eventbus.Options{})
	ctx := eventbus.ContextWithBus(context.Background(), bus)
	listening := make(chan eventbus.ServerEvent, 1)
	stopping := make(chan eventbus.ServerEvent, 1)
	stopped := make(chan eventbus.ServerEvent, 1)
	subscribe := func(topic eventbus.Topic[eventbus.ServerEvent], ch chan eventbus.ServerEvent) {
		t.Helper()
		if err := topic.Subscribe(ctx, func(_ context.Context, e *eventbus.Event[eventbus.ServerEvent]) error {
			ch <- e.Payload
			return nil
		}); err != nil {
			t.Fatalf("Subscribe %s: %v", topic.Name(), err)
		}
	}
	subscribe(eventbus.ServerListeningTopic, listening)
	subscribe(eventbus.ServerStoppingTopic, stopping)
	subscribe(eventbus.ServerStoppedTopic, stopped)

	l := NewLifecycle("debug")
	l.Bind(slog.New(slog.NewTextHandler(io.Discard, nil)), bus)
	l.Listening("127.0.0.1:6060", "")
	l.Stopping("127.0.0.1:6060", "")
	l.Stopped()

	expect := func(what string, ch chan eventbus.ServerEvent, addr string) {
		t.Helper()
		select {
		case e := <-ch:
			if e.Service != "debug" || e.Addr != addr {
				t.Fatalf("%s event = %+v, want service=debug addr=%q", what, e, addr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s event not received", what)
		}
	}
	expect("listening", listening, "127.0.0.1:6060")
	expect("stopping", stopping, "127.0.0.1:6060")
	expect("stopped", stopped, "")
}

// TestLifecycleNilBusNoop：未 Bind/脱离框架单用时事件发布 no-op，不 panic。
func TestLifecycleNilBusNoop(t *testing.T) {
	l := NewLifecycle("http")
	l.Listening("x", "")
	l.Stopping("x", "")
	l.Stopped()
	l.Bind(nil, nil)
	l.Listening("x", "")
	l.Stopping("x", "")
	l.Stopped()
}
