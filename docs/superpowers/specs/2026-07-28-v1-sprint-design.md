# Phase C v1.0 冲刺设计（文档 + API 冻结）

> 日期：2026-07-28 ｜ 对应 ROADMAP Phase C

## 背景与目标

Phase A（还债）与 Phase B（可观测性）已完成。Phase C 是 v1.0 前的最后阶段：补齐文档与示例、README 对齐现状、GoDoc 全覆盖并加 CI 强制、API 冻结准备、release 流程文档化。

## 已确认的设计决策

1. **CLI 命名保留不动**：`lynx.New` 返回的 `*CLI` 类型与 `Lynx.CLI()` 方法不改名，仅清理误导性注释。v1.0 前不引入破坏性变更。
2. **不发版**：Phase C 只交付文档与流程；v1.0.0 tag 由用户审阅后自行按 RELEASE.md 执行。
3. **docs 02-05 章**：中文教程，与 `01-introduction.md` 风格一致，代码示例取自 `_examples` 保证可编译。
4. **GoDoc 补齐 + CI 强制**：root + 4 个 contrib 模块导出符号全部补齐注释，新增根 `.golangci.yml` 启用 revive `exported` 规则防退化；`_examples` 不纳入强制。

## 详细设计

### 1. docs/ 教程（新建 4 章）

文件与内容大纲：

- `docs/02-quick-start.md`（快速开始）
  - 安装（`go get github.com/lynx-go/lynx`，注明 Go ≥ 1.25）
  - 第一个 HTTP 服务（基于 `_examples/http` 最小化版）
  - 使用配置文件（`WithSetFlagsFunc` + `WithBindConfigFunc`，yaml 示例）
  - CLI 模式（基于 `_examples/cli`，`app.CLI(cmd)` 注册命令）
  - 健康检查端点说明（`/healthz/liveness`、`/healthz/readiness`）
  - 章末"下一步"链接 03 章

- `docs/03-core-concepts.md`（核心概念）
  - 生命周期：`Init → Start → Stop` 的调用顺序与并发模型（run group）
  - Hooks：`OnStart`/`OnStop`、错误聚合（`ShutdownErrors`）
  - Options：`NewOptions`、校验规则（名称长度、ShutdownTimeout 边界）、`EnsureDefaults`
  - 配置管理：Viper 集成、flag 绑定、环境变量
  - Context helpers：`IDFromContext`/`NameFromContext`/`VersionFromContext`
  - 优雅关闭：信号处理、ShutdownTimeout

- `docs/04-component-system.md`（组件系统）
  - `Component` 接口契约（Name/Init/Start/Stop）
  - `ComponentBuilder` 与多实例（`Instances` 语义）
  - `ServerLike`/`CheckHealth` 等扩展接口
  - 自定义组件编写指南（完整可编译示例）
  - contrib 模块概览：pubsub（Broker/Router/Handler）、kafka（Binder）、schedule（Scheduler/Task）、zap（日志）

- `docs/05-servers.md`（服务器）
  - HTTP：`NewServer`、全部 Options（Addr/Timeout/HealthCheck/Logger/RequestLog/Middlewares/otel 三件套）
  - gRPC：`NewServer`、interceptors（Logging/Recovery/自定义）、健康检查、reflection
  - 可观测性接入（Phase B 成果文档化）：
    - otel provider 初始化职责在用户侧（`stdouttrace`/OTLP exporter 示例）
    - Prometheus exporter + `/metrics` 挂载
    - HTTP 中间件 `WithMiddleware` 链序（otel → requestlog → middlewares → handler）
    - `lynx.NewTraceHandler` 日志 trace_id/span_id 注入（slog/zap 两线用法）

### 2. 示例完善

- 每个 `_examples/<name>/` 新增 `README.md`：一句话说明、运行命令、关键代码点（≤ 40 行，统一模板）
- `_examples/boot/config.go`：`AppConfig` 从空 struct 填充为与 `config.yaml` 对齐的真实字段（读 config.yaml 确定字段）

### 3. README 对齐现状

- 项目结构：删除 `cli/`、`command/`、`pkg/` 目录条目，新增 `server/grpc/`、`docs/`、`boot/` 描述修正
- Go 徽章 `1.24.2+` → `1.25+`；"依赖要求"同步
- 特性列表新增：可观测性（OpenTelemetry tracing/metrics、Prometheus）
- "相关链接"：wiki 链接改为 `./docs/01-introduction.md`
- 快速开始代码示例核对可编译性（与当前 API 一致）

### 4. API 冻结准备（GoDoc + 注释清理）

- 清理残留注释（如 `lynx.go` 中 `// Run 启用 CLI` 等误导性描述），CLI 命名不动
- root 模块 + `contrib/{kafka,pubsub,schedule,zap}` 所有导出符号补齐规范 GoDoc（以 `revive exported` 规则通过为验收标准）
- 新增根 `.golangci.yml`：

```yaml
version: "2"
linters:
  enable:
    - revive
  settings:
    revive:
      rules:
        - name: exported
```

- CI 矩阵各模块运行 golangci-lint 时会向上找到根 `.golangci.yml`，无需逐模块配置
- `_examples` 模块通过配置豁免（main package 示例代码不强制导出注释）或保持默认规则即可（revive exported 默认不检查 main 包导出……需在实施时验证，若误报则在 `.golangci.yml` 中对该路径加 exclude）

### 5. RELEASE.md + Taskfile

- 新建 `RELEASE.md`：
  - 多模块 tag 约定：根模块 `vX.Y.Z`；contrib 模块 `contrib/<name>/vX.Y.Z`（Go 模块对子目录模块的 tag 要求）
  - 发版流程：`task release-all Version=vX.Y.Z Comment="release vX.Y.Z"`（Taskfile 现有 vars 参数化，实施时验证 CLI 覆盖行为）
  - 发版前检查清单：CI 全绿、ROADMAP 勾选同步、go.mod 版本引用检查（contrib 对根模块的依赖版本）
  - 发版后：更新 Taskfile `vars.Version` 默认值
- `Taskfile.yml`：仅在不支持 CLI 覆盖时调整（预期无需改动，实施时验证）

## 错误处理 / 风险

- revive `exported` 规则启用后可能暴露大量存量缺注释问题——本阶段即为此补齐；若规则对测试文件/main 包误报，在 `.golangci.yml` 配置 exclude。
- docs 中的代码示例必须从 `_examples` 现有可编译代码改篇，不新造未验证的代码。

## 兼容性

- 零代码 API 变更（仅注释/文档）；`.golangci.yml` 为新增配置文件，不影响构建。

## 非目标（YAGNI）

- 不改 CLI 命名，不打 v1.0.0 tag
- 不新增 contrib 模块，不改任何功能代码
- `_examples` 不纳入 GoDoc 强制检查
- 不写英文版文档
