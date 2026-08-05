package lynx

import (
	"context"
	"sync"
	"testing"
)

func noopHook(ctx context.Context) error { return nil }

func TestOnStartOnStopAccumulate(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	app.OnStart(noopHook, noopHook)
	app.OnStop(noopHook)

	impl := app.(*lynx)
	if got := len(impl.onStarts); got != 2 {
		t.Errorf("len(onStarts) = %d, want 2", got)
	}
	if got := len(impl.onStops); got != 1 {
		t.Errorf("len(onStops) = %d, want 1", got)
	}
}

func TestHooksConcurrent(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	const goroutines = 16
	const hooksPerGoroutine = 25

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < hooksPerGoroutine; i++ {
				app.OnStart(noopHook)
				app.OnStop(noopHook)
			}
		}()
	}
	wg.Wait()

	want := goroutines * hooksPerGoroutine
	impl := app.(*lynx)
	if got := len(impl.onStarts); got != want {
		t.Errorf("len(onStarts) = %d, want %d", got, want)
	}
	if got := len(impl.onStops); got != want {
		t.Errorf("len(onStops) = %d, want %d", got, want)
	}
}

func TestRegisterConcurrent(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.Register(&blockingComponent{name: "c"})
		}()
	}
	wg.Wait()
}
