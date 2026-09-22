package main

import "github.com/lynx-go/lynx/boot"

// Deps 是命令直接调用的类型化业务对象（图的业务出口）。
type Deps struct {
	Store *Store
}

// App 是注入器的唯一出口，双聚合：
//   - *boot.Bootstrap：框架要托管的——服务生命周期（Store 等）+ hooks；
//   - *Deps：命令要调用的——类型化业务对象。
//
// 同一批 Wire 单例的两个视图（Store 既是 Bootstrap.Services 里被托管
// 的服务，又是 Deps.Store 供命令调用）。命令选择留在图外——选哪个
// 命令是子命令框架在运行时决定的，"图里的命令"在生成期就焊死了，
// 多命令二进制必须让图只管依赖、命令函数由子命令层提供。
type App struct {
	*boot.Bootstrap
	*Deps
}

func NewApp(b *boot.Bootstrap, d *Deps) *App {
	return &App{Bootstrap: b, Deps: d}
}

func NewDeps(store *Store) *Deps {
	return &Deps{Store: store}
}
