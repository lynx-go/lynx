package lynx

// 构建信息机制：下面三个包级变量是 ldflags -X 的唯一注入目标（框架级
// 真源，应用与 debug /version 端点共用，不再各持一份）：
//
//	go build -ldflags "\
//	  -X github.com/lynx-go/lynx.BuildVersion=v1.20.0 \
//	  -X github.com/lynx-go/lynx.BuildCommit=$(git rev-parse --short HEAD) \
//	  -X github.com/lynx-go/lynx.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// 变量缺省零值（未注入即"没有构建信息"，BuildInfo.IsZero 可判定）；
// 人读兜底归 BuildInfo.Display。目标串是契约的一半——拼写错误在链接期
// 静默 no-op，注入通道由 TestBuildInfoLdflagsInjection 探针钉死。
//
// 读取面分两层：版本号的运行时读取统一走 init 的元数据兜底链（配置
// service.version → WithVersion → BuildVersion）进 Metadata，从
// lynx.Meta(ctx).Version 取解析后的生效值；需要 commit/date 的调用点
// 直接以字面量从 vars 构造 BuildInfo（不设取值函数——Go 不允许函数与
// 类型同名，名字让给三件套本体）。
var (
	// BuildVersion 是构建版本号（如 git tag）。
	BuildVersion string
	// BuildCommit 是构建对应的提交（如 git short sha）。
	BuildCommit string
	// BuildDate 是构建时间（UTC，RFC3339 建议格式）。
	BuildDate string
)

// BuildInfo 是版本三元组：需要整体持有、传递的调用点（如经依赖注入
// 收敛给接口层）以字面量从 vars 构造。未注入时字段为零值，展示层以
// Display 兜底。
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
