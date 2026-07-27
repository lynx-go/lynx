# Lynx 路线图

> 最后更新：2026-07-27

## 定位与目标

Lynx 目前为团队内部使用的 Go 微服务框架，计划对外推广开源。

**v1.0 完成标准：**

- 测试齐全：核心包与主要 contrib 模块具备单元测试，CI 强制 `-race` 与覆盖率门槛
- 文档完整：GoDoc 全覆盖、`docs/` 教程补齐、示例自带 README
- API 冻结：导出符号经过全量审查，v1.0 后保持向后兼容

## Phase A — 还债（v0.8.0）

目标：清掉存量 bug 与技术债，建立测试与 CI 安全网。

### A1 存量 bug 修复

- [x] `contrib/schedule`：`NewScheduler` 与 `Init` 重复注册任务，导致每个任务执行两遍
- [x] `contrib/pubsub`：`MessageIDFromContext` 类型断言 panic 风险；`IsRunning`/`CheckHealth` 在 `Init` 前调用 nil panic；日志中的 `context.TODO()`
- [x] `server/http`、`server/grpc`：`Timeout` 选项目前为死配置，需实际生效
- [x] `contrib/kafka`：`Consumer.Start` 中 `NewMessage` 重复调用；`NewKafkaMessageJSON` 静默忽略 `json.Marshal` 错误；`go.mod` 中 pubsub 依赖版本修正
- [x] `contrib/zap`：日志级别解析错误不再静默忽略；收敛 `slog.SetLogLoggerLevel` 全局副作用
- [x] `boot`：`onStars` 参数拼写修正；`ComponentBuilderSetFunc` nil 检查

### A2 仓库卫生

- [x] 删除误提交的 `cli.out` 与 `_examples/http/http.exe`
- [x] `.gitignore` 补充日志文件与编译产物规则

### A3 测试与 CI（本阶段重心）

- [x] 核心包：生命周期启停顺序、Hooks/addComponents 并发安全（`-race`）、优雅关闭与 OnStop 错误聚合、command 重试退避、Options 校验、context helpers
- [x] `contrib/schedule`、`contrib/pubsub` 单元测试（schedule 用测试锁住 A1 修复）
- [x] `contrib/kafka` mock 测试先行，集成测试（testcontainers）后置
- [x] GitHub Actions：多模块 `go test -race -cover` + golangci-lint + 覆盖率上传

### A4 API 精简

- [x] 移除 `pkg/errors`（与根 `errors.go` 职责重叠，仅示例引用），示例改用标准错误处理

## Phase B — 可观测性（v0.9.0）

目标：让框架从"能跑"变成"能上线"。

- [ ] OpenTelemetry tracing 接入 HTTP/gRPC（go.mod 已有 otel 间接依赖，转为显式支持）
- [ ] Prometheus metrics 中间件
- [ ] HTTP 侧最小中间件抽象（前置设计决策：当前 HTTP 直接裸 `http.Handler`，metrics/tracing 需要挂载点）
- [ ] 统一日志字段规范（trace_id 注入等），zap/slog 两条线行为一致

## Phase C — v1.0 冲刺（文档 + API 冻结）

- [ ] 补齐 `docs/` 第 02-05 章（README 已引用但不存在）
- [ ] `_examples` 各示例补 README，补完 `_examples/boot` 中空的 `AppConfig`
- [ ] GoDoc 全覆盖；README 与代码现状对齐（如 `cli/`、`command/` 目录描述）
- [ ] 全量 API 审查并冻结（含 `CLI` 命名、接口残留注释清理）
- [ ] Taskfile release 变量参数化，补 `RELEASE.md` 说明多模块打 tag 流程

## 原则

- 先还债再扩展：v1.0 前不新增 contrib 模块
- 每修一个 bug 尽量配一个回归测试
- 保持核心精简：Lynx 的价值在生命周期与组件抽象，不做大而全
