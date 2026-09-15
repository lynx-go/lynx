package lynx

import (
	"context"
	"sync"
	"testing"
)

func noopHook(ctx context.Context) error { return nil }

func noopCleanup() {}

func TestHookPhasesAccumulate(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	app.OnPreStart(noopHook, noopHook)
	app.OnDrain(noopHook)
	app.OnPreStop(noopHook)
	app.OnPostStart(noopHook)
	app.OnPostStop(noopCleanup, noopCleanup)

	impl := app.(*lynx)
	if got := len(impl.onPreStarts); got != 2 {
		t.Errorf("len(onPreStarts) = %d, want 2", got)
	}
	if got := len(impl.onDrains); got != 1 {
		t.Errorf("len(onDrains) = %d, want 1", got)
	}
	if got := len(impl.onPreStops); got != 1 {
		t.Errorf("len(onPreStops) = %d, want 1", got)
	}
	if got := len(impl.onPostStarts); got != 1 {
		t.Errorf("len(onPostStarts) = %d, want 1", got)
	}
	if got := len(impl.onPostStops); got != 2 {
		t.Errorf("len(onPostStops) = %d, want 2", got)
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
				app.OnPreStart(noopHook)
				app.OnPreStop(noopHook)
				app.OnPostStop(noopCleanup)
			}
		}()
	}
	wg.Wait()

	want := goroutines * hooksPerGoroutine
	impl := app.(*lynx)
	if got := len(impl.onPreStarts); got != want {
		t.Errorf("len(onPreStarts) = %d, want %d", got, want)
	}
	if got := len(impl.onPreStops); got != want {
		t.Errorf("len(onPreStops) = %d, want %d", got, want)
	}
	if got := len(impl.onPostStops); got != want {
		t.Errorf("len(onPostStops) = %d, want %d", got, want)
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
			app.Register(&blockingService{name: "c"})
		}()
	}
	wg.Wait()
}
