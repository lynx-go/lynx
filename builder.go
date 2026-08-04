package lynx

import (
	"context"
	"log"
)

// BuildFunc 是应用初始化回调，在 Builder 运行前执行，用于注册组件与 hooks。
type BuildFunc func(ctx context.Context, app App) error

// Builder 是 App 应用的命令行入口，封装应用实例与初始化回调。
type Builder struct {
	build BuildFunc
	app   App
}

// NewBuilder 创建 Builder 实例；初始化失败时输出错误并退出进程。
func NewBuilder(build BuildFunc, opts ...Option) *Builder {
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	app, err := newLynx(o)
	if err != nil {
		log.Fatalln(err)
	}
	return &Builder{
		build: build,
		app:   app,
	}
}

// Run 运行 Builder 应用，发生错误时输出到 stderr 并以非零状态码退出进程。
func (b *Builder) Run() {
	if err := b.RunE(); err != nil {
		log.Fatalln(err)
	}
}

// Build 运行初始化回调并返回应用实例，回调失败时返回错误。
func (b *Builder) Build() App {
	if err := b.build(b.app.Context(), b.app); err != nil {
		log.Fatalln(err)
	}
	return b.app
}

// RunE 运行 Builder 应用并返回错误，由调用方决定错误处理方式。
func (b *Builder) RunE() error {
	return b.Build().Run()
}
