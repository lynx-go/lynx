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

// Unwrap returns all collected errors, allowing errors.Is / errors.As
// to match individual hook or service errors inside the collection.
func (e *ShutdownErrors) Unwrap() []error {
	return e.Errors()
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
	// ErrSetupFuncNil 表示 NewRunner 未提供初始化回调。
	ErrSetupFuncNil = errors.New("setup func is nil")
	// ErrDrainHooksRequireDrainTimeout 表示注册了 OnDrain 钩子但未启用
	// 排水窗口（DrainTimeout=0）。v1.10.0 起排水窗口即 OnDrain 钩子的
	// 总预算（DrainHookTimeout 已并入 DrainTimeout）：窗口未启用时钩子
	// 没有执行预算，注册即配置错误——Run() 启动期快失败，好过关停期
	// 静默跳过注销的延迟暴露。
	ErrDrainHooksRequireDrainTimeout = errors.New("on-drain hooks require DrainTimeout > 0 (set WithDrainTimeout); DrainTimeout=0 disables the entire drain phase")
)
