# zap

```go
import "github.com/lynx-go/lynx/contrib/zap"
```

将 zap 高性能日志库包装为 `*slog.Logger`，提供与框架一致的日志级别与服务标识字段。

## 能力要点

- `MustNewLogger(ctx lynx.AppContext) *slog.Logger`（logger.go:20）：按应用配置创建 slog 实例，失败时 panic；`NewLogger(ctx)`（logger.go:25）为错误返回变体。
- `NewZapLogger(logLevel string, outputs ...string) (*zap.Logger, error)`（logger.go:64）：创建生产配置的 zap 实例；`outputs` 为空时输出 stdout，传文件路径即写文件。
- `NewSLogger(zlogger *zap.Logger, logLevel string) (*slog.Logger, error)`（logger.go:103）：把已有 zap 实例包装为 slog 实例。
- `NewSyncableLogger(ctx lynx.AppContext) (*SyncableLogger, error)`（logger.go:211）：slog 与 zap 双持，`Sync()`（logger.go:173）在退出前刷缓冲日志。
- `SyncOnPreStop(l *SyncableLogger) lynx.HookFunc`（logger.go:203）：生成 OnPreStop 钩子，在应用关闭前（服务 Stop 之前）刷新日志。

行为细节：

- 日志级别经 `lynx.LogLevelFromConfig` 解析：`logging.level`（规范键）→ `log-level` → `log_level`（仅配置文件的兼容回退，已废弃），均未设置时默认 `info`（logger.go:35-38）。内置 `--log-level` 显式传参覆盖配置；自定义级别 flag 须在 `WithBindConfigFunc` 里翻译进规范键（见 `_examples/boot`）。
- 级别字符串统一按 slog 域（`lynx.ParseLogLevel`）校验：接受 `warning` 别名与大小写不敏感形式，不接受 `fatal`、`info+2`。
- 自动注入 `service.id` / `service.name` / `service.version` 字段（logger.go:47-52）。
- 显式禁用 zap 生产默认采样（logger.go:72-76）：高吞吐下错误日志不会被"每 100 条只记 1 条"静默丢弃。
- 非标准 slog 级别（如 `slog.LevelError+4`）向下收敛到最近标准级别再写入（logger.go:124-140）。
- `Sync` 忽略标准流固有的良性 errno（EINVAL/EBADF/ENOTTY/EROFS），真实 I/O 错误照常上抛（logger.go:186-195）。

## 快速开始

经 `lynx.WithLoggerProvider` 一行接入（`NewLogger` 签名直接匹配，取自 `_examples/http/main.go`）：

```go
package main

import (
	"github.com/lynx-go/lynx"
	lynxzap "github.com/lynx-go/lynx/contrib/zap"
)

func main() {
	lynx.NewRunner(func(app lynx.App) error {
		app.Logger().Info("hello zap") // 输出已带 service.id/name/version
		return nil
	},
		lynx.WithName("zap-demo"),
		lynx.WithLoggerProvider(lynxzap.NewLogger),
	).Run()
}
```

provider 在配置装配后、总线构造前被框架调用：总线及其配套服务捕获到的就是 zap logger，装配期与运行期日志格式一致。

需要在退出前 flush 缓冲日志时改用 `NewSyncableLogger`（`AppContext` 不暴露钩子注册，用闭包把 `*SyncableLogger` 带到 SetupFunc 里挂 `OnPreStop`）：

```go
var syncable *lynxzap.SyncableLogger

lynx.NewRunner(func(app lynx.App) error {
	app.OnPreStop(lynxzap.SyncOnPreStop(syncable)) // 关停前 Sync
	// ...
	return nil
},
	lynx.WithLoggerProvider(func(ctx lynx.AppContext) (*slog.Logger, error) {
		l, err := lynxzap.NewSyncableLogger(ctx)
		if err != nil {
			return nil, err
		}
		syncable = l
		return l.Logger, nil // 内嵌的 *slog.Logger
	}),
).Run()
```

## 与 lynx 核心的集成

- `lynx.WithLoggerProvider(lynxzap.NewLogger)` 在构造序列内产出应用 logger；框架会同步 `slog.SetDefault`，全局默认 logger 与应用保持一致。
- `SyncOnPreStop` 返回的 `lynx.HookFunc` 注册进 `app.OnPreStop`，在服务 Stop 之前执行；v1.10.0 前名为 `SyncOnStop`（logger.go:202）。
- 级别来自应用配置键（`lynx.LogLevelFromConfig`），配置了非法级别（如 `not-a-level`）时 `NewLogger` / `NewSyncableLogger` 返回错误而非静默回退。
- 输出格式为 zap 生产 JSON（ISO8601 时间戳）；需要 trace 字段时按 `docs/05-servers.md` 5.4.6 节在 handler 外层包 `logging.NewTraceHandler`。

## 相关文档与示例

- `docs/04-service-system.md` 4.5 节「zap：日志集成」
- `docs/05-servers.md` 5.4.6 节「日志与链路关联：logging.NewTraceHandler」（zap 路线的组装方式）
- `_examples/http/main.go:24`、`_examples/schedule/main.go:18`
