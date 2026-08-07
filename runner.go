package lynx

import (
	"log"
	"sync"
)

// SetupFunc 是应用初始化回调，在 Runner 运行前执行，用于注册服务与 hooks。
type SetupFunc func(app App) error

// Runner 是 App 应用的命令行入口，封装应用实例与初始化回调。
type Runner struct {
	setup SetupFunc
	app   App
	err   error
	mu    sync.Mutex
	built bool
}

// NewRunner 创建 Runner 实例。初始化失败不会立即退出进程，
// 错误会延迟到 RunE/Run 返回，以便调用方自行处理。
// setup 为 nil 时记 ErrSetupFuncNil，由 RunE 返回。
func NewRunner(setup SetupFunc, opts ...Option) *Runner {
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	app, err := newLynx(o)
	b := &Runner{
		setup: setup,
		app:   app,
		err:   err,
	}
	if setup == nil && err == nil {
		b.err = ErrSetupFuncNil
	}
	return b
}

// Run 运行 Runner 应用，发生错误时输出到 stderr 并以非零状态码退出进程。
func (b *Runner) Run() {
	if err := b.RunE(); err != nil {
		log.Fatalln(err)
	}
}

// setupApp 运行一次初始化回调并返回应用实例与错误。回调失败或初始化失败时
// 返回 (nil, err)——调用方必须先检查错误再使用返回的 App，避免
// runner.setupApp().Register(...) 的 nil 解引用陷阱。setupApp 只执行一次回调；
// 失败后的后续调用返回同一错误（而非未初始化实例），保证契约一致。
func (b *Runner) setupApp() (App, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	if b.built {
		return b.app, nil
	}
	if b.setup == nil {
		b.err = ErrSetupFuncNil
		return nil, b.err
	}
	if err := b.setup(b.app); err != nil {
		b.err = err
		return nil, err
	}
	b.built = true
	return b.app, nil
}

// RunE 运行 Runner 应用并返回错误，由调用方决定错误处理方式。
func (b *Runner) RunE() error {
	if b.err != nil {
		return b.err
	}
	if b.app == nil {
		return ErrNotInitialized
	}
	if !b.built {
		if _, err := b.setupApp(); err != nil {
			return err
		}
	}
	if b.err != nil {
		return b.err
	}
	return b.app.Run()
}
