package lynx

import "github.com/lynx-go/lynx/buildinfo"

// BuildInfo 返回编译期注入的构建信息快照（与 Meta(ctx) 同款的取值函数
// 形态）。三元组本体与 ldflags -X 注入目标 vars 在 buildinfo 子包——
// Go 不允许函数与类型同名，类型住进子包后根包的名字让给本函数。
func BuildInfo() buildinfo.BuildInfo {
	return buildinfo.BuildInfo{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit,
		Date:    buildinfo.Date,
	}
}
