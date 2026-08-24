# Lynx 路线图

> 最后更新：2026-08-24

## 定位与目标

Lynx 目前为团队内部使用的 Go 微服务框架，计划对外推广开源。

**v1.0 完成标准：**

- 测试齐全：核心包与主要 contrib 模块具备单元测试，CI 强制 `-race`（全部 7 模块）与覆盖率门槛（根与 5 个 contrib 均 70%，`_examples` 除外）
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

- [x] OpenTelemetry tracing 接入 HTTP/gRPC（go.mod 已有 otel 间接依赖，转为显式支持）
- [x] Prometheus metrics（otel 插装 + exporter 接入）
- [x] HTTP 侧最小中间件抽象（前置设计决策：当前 HTTP 直接裸 `http.Handler`，metrics/tracing 需要挂载点）
- [x] 日志 trace 上下文注入（slog/zap 共用 logging.NewTraceHandler 装饰器）

## Phase C — v1.0 冲刺（文档 + API 冻结）

- [x] 补齐 `docs/` 第 02-05 章（README 已引用但不存在）
- [x] `_examples` 各示例补 README，补完 `_examples/boot` 中空的 `AppConfig`
- [x] GoDoc 全覆盖；README 与代码现状对齐（如 `cli/`、`command/` 目录描述）
- [x] 全量 API 审查并冻结（含 `CLI` 命名、接口残留注释清理）
- [x] Taskfile release 变量参数化，补 `RELEASE.md` 说明多模块打 tag 流程

## Phase D — v1.0 发布前审查修复（2026-08-05）

v1.0 发布前全量审查（功能缺失/设计缺陷/实现缺陷）的修复记录，详见 `CHANGELOG.md`。

- [x] **Kafka 发布阻断**：缺省 Marshaler 与 `Producer.Return.Successes` 双重缺陷
- [x] **Kafka 生产可用**：SASL/TLS 认证配置；consumer/producer 参数按侧独立
- [x] **PubSub**：Start 两阶段提交（重试不再 panic）；handler 重名提前报错；
      重试可配置；`Transport.Publish` 增加 ctx；删除遗留 Deprecated API
- [x] **核心**：服务 Stop 有界超时；Init 锁外执行；失败路径资源清理；
      OnStop 错误上抛；退出信号提前注册
- [x] **Schedule**：Stop/Start 竞态挂死修复；时区；任务错误回调
- [x] **HTTP**：脱钩 gocloud.dev/server（全部本地实现，包括 health.Checker
      抽象与 requestlog，全模块移除 gocloud.dev 依赖，见 CHANGELOG）；
      TLS/逃生口
- [x] **gRPC**：app 级健康检查同步；Recovery 最外层；流式拦截器入口
- [x] **发布卫生**：contrib go.mod bump、各模块 LICENSE、CHANGELOG、
      内部文档清理

## Phase E — v1.0 后的能力补全（v1.1+）

目标：围绕"服务间调用、流量治理、运维诊断"补齐生产通用能力。
v1.0 API 已冻结并保持向后兼容，本阶段只做增量（来源：2026-08-07
封版评审的缺口分析，参照 kratos/go-zero 等成熟框架的能力面）。

### E1 生产通用刚需（v1.1~v1.2，按优先级排序）

- [x] **EventBus 一等化**（设计见 `docs/design-eventbus.md`）：核心 `eventbus`
      （Bus/Topic/Event + wire/`Delivery`）；删 `contrib/pubsub`；`contrib/kafka`
      → `contrib/watermill-kafka`；Watermill Bus 动态订阅 + `lynx.*` 内存路由锁
- [ ] Debug/pprof 管理服务：可选 Service，挂载 `/debug/pprof/*` 与
      运行时日志级别调整，绑定内网地址/独立端口
- [ ] HTTP/gRPC client 组件：otel 插装、trace 与 `request_id`/日志属性
      传播（复用 `logging` 包）、超时/重试/错误映射默认值
- [ ] gRPC TLS 一等选项（`grpc.WithTLS`，与 HTTP 侧对齐；当前仅
      `WithServerOptions` 逃生口）
- [ ] 统一错误约定：可选 HTTP 错误响应规范 + 状态码映射 handler
- [ ] 流量治理中间件：HTTP 侧 recovery、基础限流；熔断按需

### E2 运维增强（v1.x 中后期）

- [ ] 配置热更新（viper WatchConfig）与运行时日志级别调整
- [ ] Go runtime metrics 开箱接入（goroutine/GC/内存）
- [ ] 关停排水语义显式化（readiness 先变 not-ready → 等 LB 摘流 →
      再关监听）

### E3 定位选择（按需评估，默认不做）

- 服务注册发现：contrib 形式已提供（`contrib/registry` 类型/后端/
  Registrar/Resolver + `contrib/consul` 生产后端，见 docs 第 7 章）；
  K8s 环境仍推荐 DNS/Service（ClusterIP + DrainTimeout），headless
  或裸机场景再启用注册发现
- 数据层（DB/Redis）：保持"不碰数据层"定位，docs 明确说明
- 配置中心（apollo/nacos）：按团队需要以 contrib 提供
- 脚手架 CLI（kratos-cli 类）：属开源推广工具，非框架组件

## 原则

- 先还债再扩展：v1.0 前不新增 contrib 模块
- 每修一个 bug 尽量配一个回归测试
- 保持核心精简：Lynx 的价值在生命周期与服务抽象，不做大而全
