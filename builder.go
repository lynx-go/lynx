package lynx

import (
	"context"
	"log"
	"sync"
)

// BuildFunc 是应用初始化回调，在 Builder 运行前执行，用于注册组件与 hooks。
type BuildFunc func(ctx context.Context, app App) error

// Builder 是 App 应用的命令行入口，封装应用实例与初始化回调。
type Builder struct {
	build BuildFunc
	app   App
	err   error
	mu    sync.Mutex
	built bool
}

// NewBuilder 创建 Builder 实例。初始化失败不会立即退出进程，
// 错误会延迟到 RunE/Run 返回，以便调用方自行处理。
func NewBuilder(build BuildFunc, opts ...Option) *Builder {
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	app, err := newLynx(o)
	return &Builder{
		build: build,
		app:   app,
		err:   err,
	}
}

// Run 运行 Builder 应用，发生错误时输出到 stderr 并以非零状态码退出进程。
func (b *Builder) Run() {
	if err := b.RunE(); err != nil {
		log.Fatalln(err)
	}
}

// Build 运行一次初始化回调并返回应用实例。回调失败或初始化失败时返回 nil，
// 具体错误可通过 RunE 获取。Build 只执行一次回调，重复调用返回同一实例。
func (b *Builder) Build() App {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil || b.built {
		return b.app
	}
	if err := b.build(b.app.Context(), b.app); err != nil {
		b.err = err
		return nil
	}
	b.built = true
	return b.app
}

// RunE 运行 Builder 应用并返回错误，由调用方决定错误处理方式。
func (b *Builder) RunE() error {
	if b.err != nil {
		return b.err
	}
	if b.app == nil {
		return ErrNotInitialized
	}
	if !b.built {
		b.Build()
	}
	if b.err != nil {
		return b.err
	}
	return b.app.Run()
}
