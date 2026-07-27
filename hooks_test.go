package lynx

import (
	"context"
	"sync"
	"testing"
)

func noopHook(ctx context.Context) error { return nil }

func TestHookOptionsAccumulate(t *testing.T) {
	options := &hookOptions{}

	OnStart(noopHook, noopHook)(options)
	OnStop(noopHook)(options)
	Components(&blockingComponent{name: "c"})(options)
	ComponentBuilders(&recordingBuilder{})(options)

	if got := len(options.onStarts); got != 2 {
		t.Errorf("len(onStarts) = %d, want 2", got)
	}
	if got := len(options.onStops); got != 1 {
		t.Errorf("len(onStops) = %d, want 1", got)
	}
	if got := len(options.components); got != 1 {
		t.Errorf("len(components) = %d, want 1", got)
	}
	if got := len(options.componentBuilders); got != 1 {
		t.Errorf("len(componentBuilders) = %d, want 1", got)
	}
}

func TestHooksStruct(t *testing.T) {
	h := &hooks{}
	h.OnStart(noopHook, noopHook)
	h.OnStop(noopHook)
	if got := len(h.onStarts); got != 2 {
		t.Errorf("len(onStarts) = %d, want 2", got)
	}
	if got := len(h.onStops); got != 1 {
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
	errs := make(chan error, goroutines*hooksPerGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < hooksPerGoroutine; i++ {
				if err := app.Hooks(OnStart(noopHook), OnStop(noopHook)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Hooks() error = %v", err)
	}

	want := goroutines * hooksPerGoroutine
	impl := app.(*lynx)
	if got := len(impl.onStarts); got != want {
		t.Errorf("len(onStarts) = %d, want %d", got, want)
	}
	if got := len(impl.onStops); got != want {
		t.Errorf("len(onStops) = %d, want %d", got, want)
	}
}

func TestHooksConcurrentWithComponents(t *testing.T) {
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.Hooks(Components(&blockingComponent{name: "c"})); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Hooks() error = %v", err)
	}
}
