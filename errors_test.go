package lynx

import (
	"errors"
	"sync"
	"testing"
)

func TestShutdownErrorsEmpty(t *testing.T) {
	e := &ShutdownErrors{}
	if e.HasErrors() {
		t.Error("HasErrors() = true, want false for empty collection")
	}
	if got := e.Error(); got != "" {
		t.Errorf("Error() = %q, want empty string", got)
	}
	if errs := e.Errors(); len(errs) != 0 {
		t.Errorf("Errors() = %v, want empty", errs)
	}
}

func TestShutdownErrorsAddNil(t *testing.T) {
	e := &ShutdownErrors{}
	e.Add(nil)
	if e.HasErrors() {
		t.Error("Add(nil) should be ignored")
	}
}

func TestShutdownErrorsAggregation(t *testing.T) {
	e := &ShutdownErrors{}
	e.Add(errors.New("first"))
	e.Add(errors.New("second"))

	if !e.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
	if got, want := e.Error(), "first; second"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	errs := e.Errors()
	if len(errs) != 2 {
		t.Fatalf("Errors() len = %d, want 2", len(errs))
	}
	if errs[0].Error() != "first" || errs[1].Error() != "second" {
		t.Errorf("Errors() = %v, want [first second]", errs)
	}
}

func TestShutdownErrorsErrorsReturnsCopy(t *testing.T) {
	e := &ShutdownErrors{}
	e.Add(errors.New("first"))

	errs := e.Errors()
	errs[0] = errors.New("mutated")

	if got := e.Errors()[0].Error(); got != "first" {
		t.Errorf("Errors() returned slice aliases internal state; got %q after mutation", got)
	}
}

func TestShutdownErrorsConcurrentAdd(t *testing.T) {
	e := &ShutdownErrors{}
	const goroutines = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Add(errors.New("boom"))
		}()
	}
	wg.Wait()
	if got := len(e.Errors()); got != goroutines {
		t.Errorf("len(Errors()) = %d, want %d", got, goroutines)
	}
}

func TestCommonErrors(t *testing.T) {
	if got := ErrNotInitialized.Error(); got != "service not initialized" {
		t.Errorf("ErrNotInitialized = %q", got)
	}
	if got := ErrSetupFuncNil.Error(); got != "setup func is nil" {
		t.Errorf("ErrSetupFuncNil = %q", got)
	}
}
