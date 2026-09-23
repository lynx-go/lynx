package debug

// 构建信息：默认占位值，经构建期链接参数注入，例如：
//
//	go build -ldflags "\
//	  -X github.com/lynx-go/lynx/debug.BuildVersion=v1.13.0 \
//	  -X github.com/lynx-go/lynx/debug.BuildCommit=$(git rev-parse --short HEAD) \
//	  -X github.com/lynx-go/lynx/debug.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// /version 端点输出这些值（叠加 Go/OS/Arch 与服务元数据），
// 供运维对账"线上跑的是哪个构建"。
var (
	// BuildVersion 是构建版本号（如 git tag）。
	BuildVersion = "dev"
	// BuildCommit 是构建对应的提交（如 git short sha）。
	BuildCommit = "unknown"
	// BuildDate 是构建时间（UTC，RFC3339 建议格式）。
	BuildDate = "unknown"
)
