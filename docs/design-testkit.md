# Lynx 测试套件（testkit）设计方案

| 字段 | 内容 |
| --- | --- |
| 标题 | Lynx 测试套件：`lynxtest` 包与框架可测性小幅重构 |
| 作者 | TBD |
| 日期 | 2026-09-22 |
| 状态 | **Implemented**（M0/M1/M3 已落地：R1-R5 重构、`lynxtest` 包、`_examples/testing`、`docs/08-testing.md`；M2 fake 迁移与 M4 testcontainers 未实施） |
| 适用版本 | v1.12+ |
| 相关讨论 | 三层测试模型、R1-R5 重构清单、"复用生产组装路径只换环境"原则、`WithIsolated` 全局隔离取舍、bufconn vs `:0` 回环、ROADMAP WK-19（Kafka testcontainers） |

---

## 1. 背景与问题

仓库当前没有任何共享的测试基建：无 `testutil`/`mock`/`fake` 包、无 gomock、无 testcontainers（ROADMAP Phase F 的 WK-19 尚未实施）。现存测试要么是根包的进程内生命周期测试，要么是各 contrib 模块手写的局部 fake——`fakeAppContext` 在 `boot/bootstrap_test.go:15`、`contrib/registry/registrar_test.go:19`、`contrib/consul/consul_regress_test.go:28`、`contrib/telemetry/telemetry_test.go:151`、`contrib/schedule/scheduler_test.go:520`、`contrib/zap/logger_test.go:22` 至少复制了六份。

更关键的是：**基于 Lynx 的业务应用没有任何被支持的方式在自己的 `go test` 进程里拉起一个接近生产组装方式的应用实例**。测试套件的最大价值不是测单个函数，而是测"main() 里那段组装代码"——路由、中间件链、健康检查、注册、事件订阅是否正确接线。这一层目前完全缺失。

### 1.1 现状勘察：组装路径在 `go test` 里的六类障碍

| # | 障碍 | 位置 | 后果 |
| --- | --- | --- | --- |
| 1 | 构造期解析 `os.Args` + CWD 进配置搜索路径 | `lynx.go:952-957`、`lynx.go:390-399` | 测试二进制的参数与包目录里散落的 `config.yaml` 会静默注入配置 |
| 2 | `Run()` 阻塞 + 进程级信号注册 | `lynx.go:605`、`lynx.go:644-646` | 外部包拿不到可后台运行、可确定性停止的 App（`runner.setupApp` 未导出） |
| 3 | 固定默认端口（http `:8080` / grpc `:9090`） | `server/http/server.go:28`、`server/grpc/server.go:32` | 并行测试端口冲突（`:0` + `Addr()` + `Ready()` 已支持但无编排） |
| 4 | 注册中心/发现需要真实后端 | `contrib/consul` 等 | 组件测试无法闭环（内存后端 `registry.NewMemory()` 已存在但无装配指引） |
| 5 | 进程级全局副作用：`lynx.Set` / `eventbus.SetDefault` / `slog.SetDefault` | `lynx.go:1011,1013`、`lynx.go:249,346`、`appcontext.go:12-14` | 同进程多 App（表驱动/并行）互踩全局 |
| 6 | 默认超时偏慢 | `options.go:137-184` | 全量 Run→Close 动辄数秒（实际内存总线 ~10ms 就绪；`DrainTimeout` 无下限可压小） |

### 1.2 已有的可测性基础

勘察同时确认底子不差，这是本方案"小幅重构"而非大改的依据：

- 未知 flag 容忍（`-test.*` 不炸初始化，`lynx.go:953-954`）；
- `:0` 随机端口 + `Addr()` 真实地址 + `Ready()` channel（`server/http/server.go:278-285`、`service.go:51-53`）；
- 内存注册中心 `registry.NewMemory()`（`contrib/registry/memory.go:34`），`registry.backend: memory` 配置直选（`fromconfig.go:56-59`）；
- 内存总线默认即用（`options.go:169-171`）；
- 各阶段超时全部可配，`DrainTimeout` 无最小值下限（`options.go:123-125`）；
- `Close()` 走与信号完全相同的关停路径（排水→PreStop→逆序 Stop，`lynx.go:679-720`），测试不碰信号也能验证生产关停序列。

## 2. 目标设计

### 2.1 核心原则

**测试和生产走同一条组装路径（同一个 Setup 函数），只把环境换成内存版**——配置来源、注册中心、端口、超时。环境差异只通过注入表达，组装代码里不写 `if isTest` 分支。由此保证测试不会与组装方式漂移。

### 2.2 三层测试模型

| 层 | 测什么 | 工具 | 外部依赖 |
| --- | --- | --- | --- |
| L1 单元 | biz 纯逻辑、单个 Service 的 Init/Start/Stop | `lynxtest.NewContext()`（可用 AppContext：配置/日志/Meta/Checkers 可注入）；App 级注册协议与 debug 控制面用手写替身（例外见 08-testing） | 无 |
| L2 组装测试 | 完整 App：路由、中间件链、健康检查、注册、事件 | `lynxtest.Run()`（生产 Setup + 内存环境） | 无（回环 TCP） |
| L3 集成 | consul/kafka 等真实后端 | `//go:build integration` + testcontainers | 真实容器 |

L2 是价值最大的一层，也是本方案的主体。

### 2.3 `lynxtest` API

```go
package lynxtest // github.com/lynx-go/lynx/lynxtest，零新增依赖

// L2 组装测试：生产 Setup + 内存环境，后台运行，t.Cleanup 里 Close 并等待退出
func Run(t testing.TB, setup lynx.SetupFunc, opts ...Option) lynx.App

type Option func(*opts)
    WithConfigMap(map[string]any)  // viper + Set → lynx.WithConfig 注入
    WithConfigYAML(string)         // strings.Reader → viper → 注入

// 就绪与拨号（基于 R5 的 lynx.Server 接口泛化）
func WaitReady(t testing.TB, timeout time.Duration, servers ...lynx.Server)
func HTTPClient(t testing.TB, s *http.Server) *http.Client
func GRPCConn(t testing.TB, s *grpc.Server, copts ...grpc.DialOption) *grpc.ClientConn

// L1 单元测试：替代散落各模块的手写 fakeAppContext
func NewContext(t testing.TB, opts ...ContextOption) lynx.AppContext
    ContextWithConfig / ContextWithConfigMap / ContextWithConfigYAML
    ContextWithBus / ContextWithLogger / ContextWithBusReadyTimeout
    ContextWithMeta / ContextWithCheckers   // 应用元数据（缺省 test-service）与健康检查器快照
```

`Run` 注入的基线选项：`StopTimeout/ShutdownTimeout/BusReadyTimeout = 1s`（MinTimeout 下限，实际停止很快）、`DrainTimeout = 100ms`（用 `registry.Apply` 的应用必须有非零排水窗口，否则 Run 直接失败 `ErrDrainHooksRequireDrainTimeout`；无下限校验允许压小）。

### 2.4 应用侧约定

- **main 只留三行**：`lynx.NewRunner(bootstrap.Setup, opts...).Run()`；组装逻辑放 `internal/bootstrap.Setup(app lynx.App) error`，生产与测试共用同一函数。
- 环境相关参数（地址、后端选择）一律从 `app.Config()` 读取；测试通过 `WithConfigMap` 注入 `server.addr: ":0"`、`registry.backend: memory` 等覆盖值，而非代码分支。
- Wire 用户不受影响：Setup 里 `boot.Apply(app)` 照常（`boot/bootstrap.go:63`）。

## 3. 框架侧重构（R1-R5，全部增量式，默认行为零变化）

| # | 改动 | 位置 | 为什么 | 幅度 |
| --- | --- | --- | --- | --- |
| R1 | 导出 `lynx.NewApp(opts ...Option) (App, error)` | `lynx.go:947` 的 `newLynx` | 测试与嵌入场景直接构造 App，不必借 Runner 闭包捕获 | 极小 |
| R2 | 新增 `WithConfig(cfg Config)` Option；注入时跳过 pflag 解析、文件搜索、`BindPFlags` | `lynx.go:952-957` | 从根上切断 os.Args/CWD 耦合（障碍 1）；内部 `app.c` 字段从 `*viper.Viper` 收敛为 `Config` 接口 | 小 |
| R3 | 新增 `WithIsolated()` Option：跳过 `eventbus.SetDefault`、`lynx.Set`、`SetLogger` 的 `slog.SetDefault` | `lynx.go:1011,1013`、`lynx.go:249,346` | 消除进程级全局副作用（障碍 5） | 小 |
| R4 | `http.WithListener(net.Listener)` + gRPC 同款：`Start` 优先用注入监听器 | `server/http/server.go:330`、`server/grpc/server.go:400` | 支持 bufconn：免 TCP 端口、无网络沙箱限制，服务级测试可放心并行 | 小 |
| R5 | 根包导出 `lynx.Server` 接口：`Service + Addr() + AdvertiseAddr() + Ready()` | `service.go:51` 旁 | 两个 server 已天然满足；"等就绪再拨号"辅助函数得以泛化 | 极小 |

可选的顺手修正（不阻塞落地，暂不实施）：

- `WithExitSignals()` 传空会被 `EnsureDefaults` 回填（`options.go:162`），无法真正关掉信号注册——区分"未设置"与"显式清空"；
- `contrib/registry` 的 `Registrar` 只认 `LYNX_ADVERTISE_HOST` 环境变量（`registrar.go:28`），补一个 `RegistrarOption`。

## 4. 关键决策及理由（含否掉的方案）

1. **注入 `Config` 接口，而非 `BindConfigFunc` 绕行、也非 `WithViper`**。
   否掉 `WithBindConfigFunc + ConfigSource.Set` 曲线注入：仍走 `initConfigure` 的 os.Args/文件路径，污染面没有真正关闭。否掉 `WithViper(*viper.Viper)`：把 viper 具体类型泄漏进公共选项，`Config`（`config.go:12-33`）已是公共缝隙，注入它使测试可以自带任意 Config 实现。
2. **`WithIsolated` 是布尔 Option，默认关闭；套件默认做全局 save/restore，Isolated 为显式 opt-in**。
   否掉"默认不注册全局"：破坏 v1.0 行为。否掉"套件默认开 Isolated"：app 代码可能调用 `lynx.Get()`，测试里返回 nil 会让"测试行为 ≠ 生产行为"——方向上应鼓励依赖注入，但切换必须显式。默认路径下 `Run` 在 cleanup 里恢复 `lynx.Set` / `eventbus.SetDefault` / `slog.SetDefault` 三个全局的先前值。
3. **组装测试（L2）默认 TCP `:0` 回环，bufconn（R4）服务于 server 级测试**。
   全量 bufconn 注入需要把 listener 从测试传进业务 Setup，要么全局通道要么改 Setup 签名，违背"小幅"原则；`:0` + `Ready()` + `Addr()` 已满足组装测试的并行与隔离需求（内核分配端口不冲突）。R4 的价值对象是框架内部 server 测试与直接构造 server 的用例。
4. **`WaitReady` 显式接收 server 列表，不给 App 加 `Services()` 访问器**。
   setup 闭包本就持有 server 引用，捕获即可；给 App 接口加枚举方法扩大 API 面，收益只有"少写一行捕获"。
5. **`lynxtest` 放根模块子包、零新增依赖，不放 `contrib/`**。
   contrib 位留给带外部依赖的模块（consul、zap、watermill…）；testkit 只用 stdlib 与核心既有依赖（http/grpc）。断言让用户自带 testify，mock 按需 gomock，套件不绑定。
6. **`NewContext` 提供可用的内存实现，而非 mock 桩**。
   Service 级单测需要的是"能跑起来"的 AppContext（真内存总线、真 viper 配置、接 t 的 logger），不是可打桩的接口替身；六份手写 fake 的存在证明这个缺口是真实需求。
7. **`DrainTimeout` 基线 100ms 而非 0**。
   带 `registry.Apply`（OnDrain 注销钩子）的应用在 DrainTimeout=0 时 Run 直接失败（`lynx.go:635-639` 快失败语义）；100ms 对无钩子应用也只多 100ms 关停成本，换一律可用。

## 5. 分步实施计划

| 阶段 | 内容 | 验收 |
| --- | --- | --- |
| M0 | 核心重构 R1+R2+R3+R5，server 侧 R4；每项配单测 | 存量测试 `go test -race ./...` 原样通过；默认路径行为零变化 |
| M1 | `lynxtest` 包：Run/WaitReady/HTTPClient/GRPCConn/NewContext + WithConfigMap/YAML | lynxtest 自测 dogfood 拉起带 http+grpc+内存注册中心的迷你应用 |
| M2 | `_examples` 补测试示例；contrib 手写 `fakeAppContext` 渐进迁移到 `NewContext` | 至少两个 contrib 模块完成迁移示范 |
| M3 | `docs/08-testing.md`（分层模型、目录约定、并行规则）；mise `test`/`test-integration` 任务；CI `-race -shuffle=on` + 覆盖率 70% 门槛 | 文档与 CI 生效 |
| M4 | testcontainers 基建：kafka（对应 ROADMAP WK-19）、consul | 集成测试可在 CI 真实后端上跑 |

## 6. 风险与对策（回归红线）

- **R2 是唯一的内部结构改动**（`app.c` 字段类型 `*viper.Viper` → `Config`）：所有内部取值调用点必须走 `Config` 接口方法；默认路径（flag/文件/BindPFlags）行为必须逐字节不变，`initConfigure` 专属的 `ConfigSource` 方法只在装配 viper 的局部路径使用。
- **`drain.go:13-17` 红线**：`DrainTimeout=0` 时 `HealthCheckers()` 快照与 v1.0 逐字节一致；R3 不得触碰健康聚合。
- **关停不变量不动**：信号注册时机（`lynx.go:641-646`）、post-start actor 语义（`lynx.go:740-756`）、`errors.Join` 聚合口径，全部保持原样——测试依赖 Close 路径即生产路径这一前提。
- **`WithIsolated` 不移除 ctx 内嵌总线**（`eventbus.ContextWithBus`，`lynx.go:1012`）：那是请求作用域而非进程全局，移除会改变组件行为。
- **兼容性**：全部为增量 API（新函数、新 Option、新接口），按仓库 API-freeze 哲学走 minor 版本（v1.12.0）。

## 7. 测试策略

- 每个 Option 一组针对性单测：`WithConfig` 注入后断言取值来自注入源且默认路径未读 os.Args/文件；`WithIsolated` 下三个全局未被改写、默认路径恢复原值；`WithListener` 下 `Addr()` 返回注入监听器地址、`Ready()` 语义不变。
- lynxtest 自测 dogfood：`Run` 分别拉起带 http、grpc、bufconn 双 server 的迷你应用，走完 WaitReady→拨号→请求→Close 全周期，并断言全局恢复契约；registry（memory 后端）与 AppStopped 事件的 dogfood 属 contrib/registry 模块测试范围（M2 待补，根模块不能反向依赖 contrib）。
- 全仓回归：各模块逐一 `go test -race ./...`（CLAUDE.md 既有约定）。
- 覆盖率门槛 70%（ROADMAP v1.0 既有标准）。
- **并行规则写入文档**：`lynxtest.Run` 管理的用例之间不要 `t.Parallel()`（除非显式 `WithIsolated`），一个用例一个 App（Run 单次语义，`lynx.go:618`）。

## 8. 明确不做的事

- 不引入运行时 DI 容器（wire 编译期装配保持）；
- 不改 `Run` 单次语义、不支持 restart；
- 不做 Config Watch/热更新（ROADMAP E2 另案）；
- 不做断言库、mock 生成框架、httptest 包装（handler 级测试直接用 stdlib）；
- 不做脚手架 CLI（ROADMAP Phase E3 已明确不做）；
- testcontainers（M4）另案设计，不并入本方案范围。

---

## 9. 实施记录（2026-09-22）

M0/M1/M3 已合入，全部增量 API，存量测试 `go test -race ./...` 十一个模块原样通过：

- R1-R3/R5：`lynx.go`（`NewApp`、`cfg` 字段、`SetLogger`/`applyLogLevel`/`newLynx` 的全局守卫）、`options.go`（`Options.Config`、`isolated`、`WithConfig`、`WithIsolated`）、`service.go`（`Server` 接口）；单测见 `newapp_test.go`。
- R4：`server/http/server.go` 与 `server/grpc/server.go` 的 `Options.Listener` + `WithListener`；gRPC 侧顺带扩展 `isClosedConnError` 精确匹配 bufconn 的裸 `"closed"` 错误（注入监听器的正常关停归一化）；bufconn 单测见各自 `listener_test.go`。
- lynxtest：`lynxtest/`（`Run`/`WithConfigMap`/`WithConfigYAML`/`WithOptions`、`WaitReady`/`HTTPClient`/`GRPCConn`/`BufconnHTTPClient`/`BufconnGRPCConn`、`NewContext` 系列、`LogWriter`），dogfood 自测含全局恢复契约断言。
- 实施中发现并处理的两个环境事实：本机代理环境变量会劫持回环 TCP（`HTTPClient`/`GRPCConn` 均改为直连拨号绕过代理）；`memoryBus.Start` 为阻塞语义（`NewContext` 与 `newLynx` 一致地后台启动 + 有界等待就绪）。
- 文档：`docs/08-testing.md` 使用章节、`docs/README.md` 索引、`_examples/testing` 可运行示例。

### 评审收口轮（2026-09-22，F1/F2）

三个互不可见子代理独立评审（API 库设计/测试工程 DX/正确性对抗）后落地的修正，全部未发布代码零破坏：

- **F1**：`NewApp` 构造路径对齐 `NewRunner`（空 Options → opts → newLynx，修复顺序敏感的 `WithBusOptions` 被静默丢弃，回归测试 `TestNewApp_MatchesRunnerOptionSemantics`）；gofmt 五文件；CI 加 `-shuffle=on`；mise 任务 `test`/`test-integration`（逐 workspace 模块，根下 `./...` 只覆盖根模块）；M3/dogfood 声明与实际对齐。
- **F2**：`Run` 配置分层叠加（`WithConfigBaseline` 基线 → `WithConfigYAML` → `WithConfigMap`）且**裸调用注入空配置**（包目录散落的 config.yaml 不再隐式生效，有投毒回归测试）；cleanup/panic 兜底前置（setup 失败/Goexit 也会释放应用并恢复全局）；返回句柄 `App.WaitReady/Exited/Err`（应用先于就绪退出时立即带出实际错误）；`WithTBLogger()`；`NewContext` 侧更名 `ContextWithConfigYAML` + 解析错误改测试失败、新增 `ContextWithConfig`、ctx 内嵌总线、cleanup 前置、`ContextWithBusReadyTimeout`；拨号辅助 `WithReadyTimeout`/`WithGRPCDialOptions`、空闲连接回收、`DefaultTransport` comma-ok；`WithOptions` 形参改 `los`。
- **F3（已实施，独立成文）**：评审实证的启动期竞态族（快速失败服务致兄弟 server Start 后无人关、Close 先于 Run 调度的僵尸生命周期、HTTP Stop-wins Serve 挂死、gRPC `ErrServerStopped` 未归一化）为存量框架缺陷，修复方案与实施记录见 [design-startup-race.md](design-startup-race.md)。
