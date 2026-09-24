# Lynx 路线图

> 最后更新：2026-09-24（v1.16.0 消费模型收敛：新增 Phase I，G4 WK-19 由 Phase I 承接）

## 定位与目标

Lynx 目前为团队内部使用的 Go 微服务框架，计划对外推广开源。

**v1.0 完成标准：**

- 测试齐全：核心包与主要 contrib 模块具备单元测试，CI 强制 `-race`（全部 7 模块）与覆盖率门槛（根与 5 个 contrib 均 70%，`_examples` 除外）
      （v1.0 时点口径；现已扩至 11 模块/9 个 contrib，见 ci.yml）
- 文档完整：GoDoc 全覆盖、`docs/` 教程补齐、示例自带 README
- API 冻结：导出符号经过全量审查；破坏性变更按文末"原则"节政策集中到 minor 版本发布（v1.10 / v1.11 / v1.14 / v1.15 均按此执行）

## Phase A — 还债（计划 v0.8.0，实际随 v1.0.0 发布）

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

## Phase B — 可观测性（计划 v0.9.0，实际随 v1.0.0 发布）

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
- [x] 发版任务参数化（Taskfile → mise），补 `RELEASE.md` 说明多模块打 tag 流程

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

## Phase F — 全量审查修复（v1.6.0，2026-08-25）

全量架构与代码审查（83 项发现：81 修复 + 2 约定不修；2 Critical /
12 High / 24 Medium，Low 计数以 `docs/review-2026-08-25.md` 终态为准），
两轮修复 + 复审闭环，逐项明细见 `docs/review-2026-08-25.md`，摘要见
`CHANGELOG.md` v1.6.0。

- [x] **Critical**：client/http 默认超时致 body 不可读（cancelTimerBody 模式）；
      Kafka 同 topic 多 handler 同组静默瓜分消息（Subscribe 拦截 + 语义文档）
- [x] **High**：memoryBus Publish/Stop 竞态 panic；毒消息无限重投（max_redeliveries）；
      健康检查全链路超时+并发；panic/错误详情泄露；Consul Register 无超时；
      裸 `:port` 契约三处对齐；zap 默认采样丢日志；总线就绪 1s 硬编码
- [x] **复审二轮**：复审发现 16 个修复引入的新问题（groupClaims 不回滚、
      重投计数跨组互踩、forwardAck 30s 误伤慢 handler、Status 负数 panic 等），
      全部修复并以变异验证测试锁住
- [x] 测试盲区补齐：healthz 端点、HTTP TLS、超时×body、zap 内容断言、
      schedule 时区、consul index 回绕与挂死 agent、watcher 错误退避
- [ ] 后续工作：Kafka testcontainers 集成测试（WK-19，移入 G4）
      ——同列的 Resolver 订阅 API 与 command 健康等待上界可配化
      已随 v1.13.0 完成

## Phase E — v1.0 后的能力补全（v1.1+）

目标：围绕"服务间调用、流量治理、运维诊断"补齐生产通用能力。
核心生命周期 API 自 v1.0 冻结；历史上另有若干破坏性更名（v1.10 钩子
更名、v1.11 `Bind`→`Apply`、v1.14/v1.15 架构收敛批次等），均在 CHANGELOG
明示且无兼容别名，政策见文末"原则"节。（来源：2026-08-07 封版评审的缺口分析，参照
kratos/go-zero 等成熟框架的能力面。）

### E1 生产通用刚需（v1.1+，按优先级排序）

- [x] **EventBus 一等化**（v1.5.0 落地，设计见 `docs/design-eventbus.md`）：
      核心 `eventbus`（Bus/Topic/Event + wire/`Delivery`）；删 `contrib/pubsub`；
      `contrib/kafka` → `contrib/watermill-kafka`；Watermill Bus 动态订阅 +
      `lynx.*` 内存路由锁
- [x] Debug/pprof 管理服务（v1.1.0，新包 `debug`）：挂载 `/debug/pprof/*`
      与 `/healthz`，缺省仅本机回环 `127.0.0.1:6060`；
      运行时日志级别调整未包含，移入 G1
- [x] HTTP/gRPC client 组件（v1.1.0）：otel 插装、trace 与 `request_id`/
      `user_id` 日志属性传播、默认超时与重试（HTTP 侧指数退避）；
      遗留 gRPC 服务端 request_id 还原闭环，移入 G3
- [x] gRPC TLS 一等选项（v1.1.0）：server/client 两侧 `WithTLSConfig`，
      与 HTTP 侧对齐
- [x] 统一错误约定（v1.1.0）：`server/http` 的 `StatusError` +
      `DefaultErrorHandler`，统一 JSON 错误体与状态码映射
- [x] 流量治理中间件（v1.1.0）：HTTP 侧 recovery、基础限流（`RateLimit`），
      gRPC 侧 Recovery 拦截器；熔断自 G2 转正立项

### E2 运维增强（v1.x 中后期）

- [x] 配置热更新（viper WatchConfig）与运行时日志级别调整（移入 G1）
- [x] Go runtime metrics 开箱接入（goroutine/GC/内存）（移入 G1）
- [x] 关停排水语义显式化（readiness 先变 not-ready → 等 LB 摘流 →
      再关监听；v1.1 引入，v1.10.0 将 OnDrain 钩子预算并入 `DrainTimeout` 窗口）

### E3 定位选择（按需评估，默认不做）

- 服务注册发现：contrib 形式已提供（`contrib/registry` 类型/后端/
  Registrar/Resolver + `contrib/consul` 生产后端，见 docs 第 7 章）；
  K8s 环境仍推荐 DNS/Service（ClusterIP + DrainTimeout），headless
  或裸机场景再启用注册发现
- 数据层（DB/Redis）：保持"不碰数据层"定位（定位边界尚未写入 docs，
  随 G3 文档债补齐）
- 配置中心（apollo/nacos）：按团队需要以 contrib 提供
- 脚手架 CLI（kratos-cli 类）：属开源推广工具，非框架组件
- 动态插件机制：保持编译期 Service/ServiceFactory + contrib module 的
  扩展方式，不做运行时插件加载

## Phase G — v1.13+ 能力补全（2026-09-22 制定）

对账说明：v1.2~v1.12 的特性演进（watermill-kafka、registry/consul、
cluster、boot/registry `Apply` 更名、lynxtest 可测性套件等）未在本
路线图逐期立 Phase，明细见 `CHANGELOG.md`。本阶段基于 2026-09-22 的
能力面盘点与三路独立复核，延续"生命周期 + 通信 + 可观测"主干做
补全，不开新的大模块；对账同时回收了 v1.1.0 CHANGELOG 与代码注释中
两个"定位 v1.2"的失联子承诺（按维度限流、服务器级默认 ErrorHandler，
见 G2/G3）。推进建议：安全网与小项先行——G4 假件、WK-19、G5 小项
可并行启动；主线 G1 → G2 → G3（内部运维价值优先）；开源推广启动则
G3 提前，且先启动其"开源准备"子列。

### G1 运行时可调性与可观测（服务跑起来之后还能调、还能看）

- [x] 运行时日志级别调整与构建信息：挂 `debug/` 服务端点（与 pprof
      同域，复用既有本机回环安全边界），含 `/version` 构建信息
      （ldflags 注入）
- [x] 配置热更新：viper WatchConfig 桥接 eventbus（发 `lynx.*` 主题，
      沿用框架生命周期事件先例），订阅方自行选择响应粒度；
      设计时注意与三级就绪解析（`lynx.Ready`，v1.12）的语义协同
- [x] Go runtime metrics 开箱接入：otel `instrument/runtime` 接进
      `contrib/telemetry`（goroutine/GC/内存），含容器 CPU 配额感知
      （automaxprocs 类，K8s 配额下修正 GOMAXPROCS）
- [ ] `/metrics` 一等挂载选项（当前需自行手挂 promhttp，
      `contrib/telemetry` 注释亦如此指引，`_examples/http` 为手挂示例）
- [ ] 总线消息 trace 上下文传播：消息头带 W3C traceparent，跨进程
      Bus 追踪不断链（现仅传播 `request_id/user_id` 日志属性白名单）
- [ ] telemetry 配置驱动与 OTLP：OTLP exporter/采样一等选项、
      `NewFromConfig` 装配（对齐 bus:/kafka:/registry: 惯例）
- [ ] Kafka consumer lag 指标导出（watermill-kafka 接入生产后的
      第一监控诉求）

### G2 流量韧性（出站治理与入站 gRPC 对齐）

- [x] 熔断器：`client/http` 已有超时 + 重试退避，补熔断（E1 "按需"
      转正）
- [x] 按路由/IP/用户维度限流（v1.1.0 CHANGELOG 承诺回收，现仅
      服务器级单桶）；HTTP 请求体大小上限（MaxBytesReader 类
      防御默认值）
- [ ] gRPC server 侧限流/超时拦截器（与 HTTP 侧对齐，现仅
      Recovery/Logging）
- [ ] gRPC client 侧重试/负载策略评估（可先只出结论不动代码）；
      `client/http` Transport/连接池调优逃生口
- [x] gRPC 服务端 request_id 还原闭环（client 已写入 metadata，
      `client/grpc` 注释标注 backlog）

### G3 缺陷清偿与开源准备

缺陷清偿（用户可感知）：

- [x] 文档与发布卫生债：9 个 contrib 模块补 README（对齐 `docs/`
      教程写法；watermill-kafka 可用 `_examples/bus-kafka` 改写）；
      `_examples/bus` 补 README（v1.5.0 新增示例漏配）；
      `contrib/watermill` 补 LICENSE（9 个 contrib 中唯一缺失）；
      docs 补定位边界说明（数据层不做等）
- [x] 服务器级默认 ErrorHandler Option（v1.1.0 承诺，
      `server/http/errors.go` 注释自标待做）
- [x] docs 写明 gRPC reflection 常开的取舍（现 NewServer 即注册、
      无开关；开源后必被问及）

开源准备（对外推广启动时优先）：

- [x] 开源协作基建：CONTRIBUTING.md（面向人的贡献指南，CLAUDE.md
      面向 agent 不能替代）、SECURITY.md、issue/PR 模板
      （`.github/` 目前仅 CI）
- [ ] 若面向国际社区：文档/README 英文化（战略决策，视推广目标
      而定，另需评估双语维护成本）

### G4 测试与安全网（延续 v1.12 lynxtest 方向）

- [x] schedule/bus 测试假件或时钟注入（G1 配置热更新的测试前置，
      建议与 G1 同期或先行；v1.15.0 起统一为公开 `lynx.Clock` 接缝 +
      `internal/clock.Fake`——cluster 续约/TTL 与 registry 缓存 stale
      均可确定性推进，见 Phase H）
- [ ] Kafka testcontainers 集成测试（WK-19，Phase F 遗留承接，
      已积压月余，建议尽早；由 Phase I 承接推进），模式沉淀为 contrib 可复用的测试辅助
- [x] CI 增加 govulncheck 依赖漏洞扫描（开源后供应链关注度陡增，
      v1.10.0 的 grpc CVE 修复说明风险面真实）

### G5 存量改进（Phase F 遗留转正）

- [x] Resolver 订阅 API：消除 gRPC 发现 5s 轮询（跨 registry 与
      gRPC resolver 的设计项，先出设计再动手）
- [x] command 健康等待上界可配化（小项，随手带走不占阶段位）

### 按需 contrib（只立原则，不立项）

etcd registry、Nacos/Apollo 配置中心、RabbitMQ/NATS transport、
认证/鉴权中间件（JWT/API-key/服务间身份，middleware 扩展点已具备）、
Outbox 发送盒与 DLQ 死信转投（总线故事的自然延伸，watermill 生态有
forwarder 组件）、CORS/gzip 等通用中间件、OTLP Logs（可观测三支柱
缺一）：有真实使用需求再以 contrib 收录，不做能力面竞赛。

## Phase H — 架构收敛（v1.15.0，2026-09-24）

以 2026-09-23 架构评审报告的候选 1-8 为主线的重构批次（明细与迁移
总览见 `CHANGELOG.md` v1.15.0）：不做能力扩展，只把重复规则收敛成唯一
模块、把注释级不变量变成机制、把测试从 sleep 改为确定性断言。

- [x] 应用生命周期自有关停调度（`lifecycle.go`；移除 oklog/run；阶段
      顺序与 actor 注册顺序解耦、服务 Stop 统一 LIFO）
- [x] readiness 有界收敛（Ready 等待与 CheckHealth 调用不越预算；修复
      OrderedServices 与总线就绪两处可永久挂起的路径）
- [x] Bus 共享核心下沉（`eventbus.Resolver` / `InvokeHandler` /
      `GroupClaims` / `RedeliveryLimiter`；watermill 只接线）
- [x] server 共享规则收敛至 `internal/serverkit`（健康执行 / 有界关停 /
      请求标识 / 生命周期事件；`lynx.server.*` 主题统一，HTTP 默认传播）
- [x] Watcher 骨架与订阅契约（`registry.WatcherBase[T]`；
      `Resolver.Subscribe(name, filter)`；sentinel 统一）
- [x] 时间源接缝（公开 `lynx.Clock` + `internal/clock.Fake`；cluster
      租约与 registry 缓存边界确定性断言）
- [x] lynxtest 补全为唯一测试上下文（Meta / Checkers 注入；7 个手写
      AppContext 替身迁移删除）
- [x] 剩余小项：schedule identity 与引擎 parser 同源、`Topic.PublishRaw`
      等价面删除（原始载荷单入口）、`telemetry.Options` / `schedule.Options`
      未导出、`WithMetadata` 克隆修复
- [ ] 时钟接缝向其余真实 ticker 延伸（campaign 退避、grpc 兜底轮询、
      dns 轮询、registrar 心跳/探测）——有确定性测试需求时再接，避免
      无消费方的管道

本批明确不做（有意保留，评审记录在案）：

- `Config` / `ConfigSource` 的 17 方法直通接口：viper 替换接缝是刻意
  设计，代价（自定义实现与测试替身需写全方法）由 lynxtest 的配置注入
  覆盖绝大部分；
- App 级注册协议替身（boot/fromconfig）与 debug `/loglevel` 控制面：
  非 AppContext 适用面，保持手写。

## Phase I — 消费模型收敛（v1.16.0，2026-09-24）

以「订阅单元 = 事件」为核心的破坏性收敛（迁移总览见 `CHANGELOG.md`
Unreleased，设计与理由见 `docs/design-eventbus-consumption.md`）：

- [x] 订阅复用 + 进程内扇出（watermill `subscription.go` 订阅注册表与
      订阅级 dispatcher；聚合确认：全成功 Ack、超限 handler 跳过止损、
      per-handler 成功清计数）
- [x] 组 / 消费者成员数下沉后端：删 `WithGroup` / `WithInstances` /
      `WithTopicGroup` / `WithTopicInstances`、`GroupClaims` /
      `EffectiveGroup` / `DefaultGrouper`、`Transport.DeliveryMode`、
      kafka `DefaultGroup`；订阅复用键 = 逻辑 topic
- [x] 订阅级在途上限 `bus.topics.<t>.max_in_flight`（默认 1 = 串行且保序；
      限流点在适配器，防 router 每消息 goroutine 无界堆积并形成背压）
- [x] `lynx.NewHandlerService` / `EventHandler[T]`：订阅型 handler 的
      Service 适配器（先 Init 注入依赖再订阅）
- [x] handler 超时（`bus.handler_timeout` / `bus.topics.<t>.handler_timeout`）：
      单次尝试超时 → 终态失败 → 重试 / 重投 / 毒消息止损，防挂死 handler
      永久占槽（实现归 `eventbus.InvokeHandler`：截止 ctx + 看门狗）
- [ ] Kafka 提交乱序窗口：文档明示已完成（design R8）；按分区最低未确认
      offset 提交待评估（需 transport 感知 partition）
- [ ] Kafka testcontainers 集成测试（承接 G4 WK-19）：真 broker 钉住订阅
      复用、组 / 成员数只来自配置、`max_in_flight` 的提交顺序

## 原则

- 先还债再扩展：v1.0 前不新增 contrib 模块
- 每修一个 bug 尽量配一个回归测试
- 保持核心精简：Lynx 的价值在生命周期与服务抽象，不做大而全
- contrib 按需收录：有真实需求才新增 contrib 模块，不做 catalogue 竞赛
- 破坏性变更政策：核心生命周期 API 保持稳定；确需破坏性更名时，
  CHANGELOG 明示（不带兼容别名）、示例与 docs 同步更新，集中在
  minor 版本发布并经评审
- ROADMAP 随版对账：每次发版时同步勾选/更新对应条目，避免规划文档
  与代码现状脱节（2026-08-25 后曾滞后月余，2026-09-22 对账时 E1
  多项已实现未勾选）
