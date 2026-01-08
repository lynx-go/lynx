package lynx

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestOnStart(t *testing.T) {
	t.Run("single hook", func(t *testing.T) {
		var called bool
		option := OnStart(func(ctx context.Context) error {
			called = true
			return nil
		})

		opts := &hookOptions{}
		option(opts)

		if len(opts.onStarts) != 1 {
			t.Errorf("expected 1 onStart hook, got %d", len(opts.onStarts))
		}

		if !called {
			ctx := context.Background()
			if err := opts.onStarts[0](ctx); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !called {
				t.Error("expected hook to be called")
			}
		}
	})

	t.Run("multiple hooks", func(t *testing.T) {
		callCount := 0
		option := OnStart(
			func(ctx context.Context) error {
				callCount++
				return nil
			},
			func(ctx context.Context) error {
				callCount++
				return nil
			},
		)

		opts := &hookOptions{}
		option(opts)

		if len(opts.onStarts) != 2 {
			t.Errorf("expected 2 onStart hooks, got %d", len(opts.onStarts))
		}

		ctx := context.Background()
		for _, hook := range opts.onStarts {
			if err := hook(ctx); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}

		if callCount != 2 {
			t.Errorf("expected callCount = 2, got %d", callCount)
		}
	})
}

func TestOnStop(t *testing.T) {
	t.Run("single hook", func(t *testing.T) {
		var called bool
		option := OnStop(func(ctx context.Context) error {
			called = true
			return nil
		})

		opts := &hookOptions{}
		option(opts)

		if len(opts.onStops) != 1 {
			t.Errorf("expected 1 onStop hook, got %d", len(opts.onStops))
		}

		if !called {
			ctx := context.Background()
			if err := opts.onStops[0](ctx); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !called {
				t.Error("expected hook to be called")
			}
		}
	})

	t.Run("multiple hooks", func(t *testing.T) {
		callCount := 0
		option := OnStop(
			func(ctx context.Context) error {
				callCount++
				return nil
			},
			func(ctx context.Context) error {
				callCount++
				return nil
			},
		)

		opts := &hookOptions{}
		option(opts)

		if len(opts.onStops) != 2 {
			t.Errorf("expected 2 onStop hooks, got %d", len(opts.onStops))
		}

		ctx := context.Background()
		for _, hook := range opts.onStops {
			if err := hook(ctx); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}

		if callCount != 2 {
			t.Errorf("expected callCount = 2, got %d", callCount)
		}
	})
}

func TestComponents(t *testing.T) {
	comp1 := &mockComponent{name: "comp1"}
	comp2 := &mockComponent{name: "comp2"}

	option := Components(comp1, comp2)
	opts := &hookOptions{}
	option(opts)

	if len(opts.components) != 2 {
		t.Errorf("expected 2 components, got %d", len(opts.components))
	}

	if opts.components[0] != comp1 {
		t.Error("expected first component to be comp1")
	}

	if opts.components[1] != comp2 {
		t.Error("expected second component to be comp2")
	}
}

func TestComponentBuilders(t *testing.T) {
	builder1 := &mockComponentBuilder{instances: 2}
	builder2 := &mockComponentBuilder{instances: 3}

	option := ComponentBuilders(builder1, builder2)
	opts := &hookOptions{}
	option(opts)

	if len(opts.componentBuilders) != 2 {
		t.Errorf("expected 2 component builders, got %d", len(opts.componentBuilders))
	}

	if opts.componentBuilders[0] != builder1 {
		t.Error("expected first builder to be builder1")
	}

	if opts.componentBuilders[1] != builder2 {
		t.Error("expected second builder to be builder2")
	}
}

func TestHooksInterface(t *testing.T) {
	h := &hooks{}

	var startCalled, stopCalled bool
	h.OnStart(func(ctx context.Context) error {
		startCalled = true
		return nil
	})

	h.OnStop(func(ctx context.Context) error {
		stopCalled = true
		return nil
	})

	if len(h.onStarts) != 1 {
		t.Errorf("expected 1 onStart, got %d", len(h.onStarts))
	}

	if len(h.onStops) != 1 {
		t.Errorf("expected 1 onStop, got %d", len(h.onStops))
	}

	ctx := context.Background()
	if err := h.onStarts[0](ctx); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !startCalled {
		t.Error("expected onStart hook to be called")
	}

	if err := h.onStops[0](ctx); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !stopCalled {
		t.Error("expected onStop hook to be called")
	}
}

func TestHookErrorHandling(t *testing.T) {
	t.Run("onStart returns error", func(t *testing.T) {
		expectedErr := errors.New("start error")
		option := OnStart(func(ctx context.Context) error {
			return expectedErr
		})

		opts := &hookOptions{}
		option(opts)

		ctx := context.Background()
		err := opts.onStarts[0](ctx)

		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})

	t.Run("onStop returns error", func(t *testing.T) {
		expectedErr := errors.New("stop error")
		option := OnStop(func(ctx context.Context) error {
			return expectedErr
		})

		opts := &hookOptions{}
		option(opts)

		ctx := context.Background()
		err := opts.onStops[0](ctx)

		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})
}

func TestHooksConcurrentAccess(t *testing.T) {
	h := &hooks{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			if i%2 == 0 {
				h.OnStart(func(ctx context.Context) error {
					return nil
				})
			} else {
				h.OnStop(func(ctx context.Context) error {
					return nil
				})
			}
		}(i)
	}

	wg.Wait()

	if len(h.onStarts) != 50 {
		t.Errorf("expected 50 onStart hooks, got %d", len(h.onStarts))
	}

	if len(h.onStops) != 50 {
		t.Errorf("expected 50 onStop hooks, got %d", len(h.onStops))
	}
}
