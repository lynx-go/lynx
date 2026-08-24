package lynx

import (
	"context"
	"testing"
	"time"

	"github.com/lynx-go/lynx/eventbus"
)

// TestAppStoppedDeliveredBeforeBusStop 关停：AppStopped 在 Bus.Stop 前可被订阅收到。
func TestAppStoppedDeliveredBeforeBusStop(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx: %v", err)
	}
	gotStopped := make(chan struct{}, 1)
	if err := eventbus.AppStoppedTopic.Subscribe(app.Context(), "test-app-stopped", func(ctx context.Context, e *eventbus.Event[eventbus.AppEvent]) error {
		gotStopped <- struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	time.Sleep(50 * time.Millisecond)
	app.Close()
	select {
	case <-gotStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive AppStopped — Bus must still be usable before Stop")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
