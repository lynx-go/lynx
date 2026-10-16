// Package buildinfo 承载编译期注入的构建信息：三个包级变量是 ldflags -X
// 的唯一注入目标（框架级真源，应用与 debug /version 端点共用，不再各持
// 一份）：
//
//	go build -ldflags "\
//	  -X github.com/lynx-go/lynx/buildinfo.Version=v1.20.0 \
//	  -X github.com/lynx-go/lynx/buildinfo.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/lynx-go/lynx/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// 变量缺省零值（未注入即"没有构建信息"，BuildInfo.IsZero 可判定）；人读
// 兜底归 BuildInfo.Display。根包的 lynx.BuildInfo() 是取值函数（函数与
// 类型不可同名，类型住在子包、根包名字让给函数）。目标串是契约的一半
// ——拼写错误在链接期静默 no-op，注入通道由 TestBuildInfoLdflagsInjection
// 探针钉死。
package buildinfo

// 构建信息三件套：ldflags -X 注入目标（包名已带语境，变量不再重复
// Build 前缀）。
var (
	// Version 是构建版本号（如 git tag）。
	Version string
	// Commit 是构建对应的提交（如 git short sha）。
	Commit string
	// Date 是构建时间（UTC，RFC3339 建议格式）。
	Date string
)

// BuildInfo 是版本三元组：lynx.BuildInfo() 产出，应用可直接持有、传递
// （如经依赖注入收敛给接口层）。未注入时字段为零值，展示层以 Display
// 兜底。
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// IsZero 报告是否完全没有注入版本信息。
func (b BuildInfo) IsZero() bool {
	return b.Version == "" && b.Commit == "" && b.Date == ""
}

// Display 返回人读版本串：Version 为空兜底 "dev"（go run / 测试形态）；
// Commit 非空时追加 "(commit …[, built …])" 修饰。
func (b BuildInfo) Display() string {
	version := b.Version
	if version == "" {
		version = "dev"
	}
	if b.Commit == "" {
		return version
	}
	line := version + " (commit " + b.Commit
	if b.Date != "" {
		line += ", built " + b.Date
	}
	return line + ")"
}
