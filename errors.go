package lynx

import (
	"errors"
	"strings"
	"sync"
)

// ShutdownErrors collects errors that occur during shutdown.
// It is safe for concurrent use.
type ShutdownErrors struct {
	mu     sync.Mutex
	errors []error
}

// Add appends an error to the collection. Nil errors are ignored.
func (e *ShutdownErrors) Add(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errors = append(e.errors, err)
}

// Error returns a semicolon-separated string of all collected errors.
func (e *ShutdownErrors) Error() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.errors) == 0 {
		return ""
	}
	var msgs []string
	for _, err := range e.errors {
		msgs = append(msgs, err.Error())
	}
	return strings.Join(msgs, "; ")
}

// HasErrors returns true if any errors have been collected.
func (e *ShutdownErrors) HasErrors() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.errors) > 0
}

// Errors returns a copy of all collected errors.
func (e *ShutdownErrors) Errors() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]error, len(e.errors))
	copy(result, e.errors)
	return result
}

// Common errors that can be used throughout the framework.
var (
	// ErrNotInitialized 表示服务在 Init 之前被使用（如 Command 在未注册时直接 Start）。
	ErrNotInitialized = errors.New("service not initialized")
	// ErrBuildFuncNil 表示 NewBuilder 未提供初始化回调。
	ErrBuildFuncNil = errors.New("build func is nil")
)
