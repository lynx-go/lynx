# Lynx 启动期竞态修复设计方案

| 字段 | 内容 |
| --- | --- |
| 标题 | 启动期关停交错（Close/interrupt 与服务 Start 的竞态族）修复 |
| 作者 | TBD |
| 日期 | 2026-09-22 |
| 状态 | **Implemented**（2026-09-22 实施，R-D/R-C/R-A/R-B 全部落地并附竞态回归测试；见第 7 节实施记录） |
| 适用版本 | v1.12+ |
| 相关讨论 | oklog/run interrupt 语义、`service.go` 的 Stop 容忍契约、testkit 负向启动测试场景、K8s SIGTERM-at-startup |

---

## 1. 现状与问题

根因是同一族：**oklog/run 的 interrupt 可以先于某个服务 actor 的 execute 执行体运行**（首个 actor 返回后，按注册顺序同步执行各 interrupt，而慢启动的 execute 可能尚未调度）；`lynxtest.Run` 的 `Close`（经 `cancelCtx` 触发 shutdown actor）与后台 `Run` goroutine 之间也有同构的启动期交错。四个已实证的缺口：

| # | 缺陷 | 位置 | 实证 |
| --- | --- | --- | --- |
| D1 | 快速失败的服务触发 run.Group 中断后，兄弟 server 的 Stop 先于其 Start 执行；Stop 见零值字段无害返回，Start 随后完成 Listen 进入 Serve，**此后无人再关它**——Run 永不返回、监听 goroutine 泄漏 | `lynx.go` addServices 的 execute 闭包（Start 前无中断检查） | 5 次复现 2 次命中（评审 B 探针） |
| D2 | `Close` 在 `Run` goroutine 被调度前完成：Close 走 running=false 路径停总线、清 post-stop；Run 随后照常执行完整生命周期（僵尸生命周期），Start 中订阅已关闭总线报错 → cleanup 伪失败 | `lynx.go` Close/Run 无 closed 状态 | 评审 C 探针复现（testkit 快速失败用例场景） |
| D3 | HTTP server：Stop-wins 交错下 Stop 见 `httpServer==nil` 早退，Start 随后 `Serve` 永不返回（挂死 + 泄漏） | `server/http/server.go` Stop/Start | 评审 C 线性化复现；生产等价场景为启动期收到 SIGTERM |
| D4 | gRPC server：Stop-wins 交错下 `Serve` 返回 `grpc.ErrServerStopped` 未被归一化 → 正常关停被误报为服务失败（发布虚假 `lynx.service.failed` 事件） | `server/grpc/server.go` 归一化分支 | 评审 C 线性化复现 |

这四个缺口都是**存量框架问题**（非 testkit 引入），但 testkit 的核心场景——负向启动测试（"DB 连不上 → 应用应启动失败"）与快速失败用例——会高频踩中，在 CI 上表现为 30 秒级挂起或误导性失败。

既有契约只覆盖了一半：`service.go:29-31` 要求 **Stop 容忍先于 Start 被调用**；缺失的对称面是**已中断（Stop 已请求）后不得再执行 Start**。

## 2. 目标设计

四个修复点，按风险从小到大：

- **R-D（一行）**：gRPC 归一化分支补 `errors.Is(serveErr, grpc.ErrServerStopped)`
  （仅 `stopRequested` 已置位时），对齐 grpc-go 上游"Serve-after-Stop 定义
  为哨兵错误"的建模。
- **R-C（局部）**：HTTP server 镜像 gRPC 侧既有的 `stopRequested` 模式：
  Stop 先置原子标志再读 `httpServer`；Start 在存入 httpServer 之后、进入
  `Serve` 之前检查该标志，命中则不 Serve、复位 started、返回 nil。与
  `s.mu` 的读写构成闭合窗口。
- **R-A（生命周期核心）**：`lynx` 增加 `closed` 状态：Close 在持 `app.mu`
  段内置位（与 Run 侧 `running.Swap(true)` 同一互斥域），Run 入口持锁检查
  `closed` → 直接返回新增的 `ErrAppClosed`，不再执行任何钩子/服务——
  闭合 `Close 即释放`（Run 从未启动时）的文档契约，消除 D2 的僵尸路径。
- **R-B（生命周期核心）**：addServices 的 execute 闭包在调用
  `service.Start` 前检查中断状态（与 interrupt 侧在同一互斥/原子序下），
  已中断则跳过 Start——消除 D1。实现选择（中断标志 vs per-actor started
  channel）牵涉 `startWG`/登记事务语义，实施前需专项推演。

## 3. 关键决策及理由

1. **框架层修，不做 testkit 侧屏障**（评审 C 曾提出 harness 屏障作为最小
   缓解）：D1/D3/D4 同时影响生产（启动期 SIGTERM），只在测试工具里挡住
   是双层补丁；框架修好后 testkit 无需额外机制。
2. **哨兵错误而非静默吞掉**：`ErrAppClosed` 显式返回，调用方能区分
   "从未运行" 与 "运行后正常退出"；gRPC 侧沿用上游 `ErrServerStopped`
   的归类方式。
3. **不动既有不变量**：关停顺序（排水→PreStop→逆序 Stop→总线→PostStop）、
   `startWG` 触发界、post-start actor 语义、`errors.Join` 聚合口径全部
   保持——本方案只在"启动尚未完成"的窗口内补守卫。

## 4. 风险与对策

- R-A/R-B 触碰 `running`/`startWG`/登记事务的互斥设计，是 `review-2026-08-25.md`
  之后对生命周期并发语义的最大改动：实施时先写竞态单测（`failStartService`
  + 真 server 的 D1 复现、Close-before-schedule 的 D2 复现、两 server 的
  D3/D4 线性化复现），再动实现；`-race -shuffle=on` 全量回归。
- 存量测试可能出现顺序敏感的暴露：新增守卫改变"僵尸生命周期也能跑完"的
  隐式行为，个别依赖该行为的测试需按新契约修正。

## 5. 测试策略

- 单测：四个缺口各一条可复现用例（构造 `failStartService` 与 server 组合、
  先 Close 后调度 Run 的线性化交错、Stop-wins-then-Start 的 server 级用例）；
- 契约测试：`Stop 容忍先于 Start` 与 `中断后不 Start` 的对称面断言；
- 回归：11 模块 `-race -shuffle=on`，重点观察既有生命周期测试
  （`lynx_test.go`、`lifecycle_events_test.go`）无语义漂移。

## 6. 明确不做的事

- 不引入 App 重启语义（Run 单次契约不变）；
- 不为 oklog/run 换替代并发原语；
- 不在本方案内做 `WithExitSignals` 显式清空等无关修正（见
  design-testkit.md 的可选项清单）。

---

## 7. 实施记录（2026-09-22）

四个修复点全部落地：

- **R-D**：`server/grpc/server.go` 归一化分支补 `errors.Is(serveErr, grpc.ErrServerStopped)`（仅 `stopRequested` 已置位时生效）。
- **R-C**：`server/http/server.go` 新增 `stopRequested atomic.Bool`：Stop 在读取 httpServer 之前置位；Start 在存入 listener 之后、listening 日志/事件与 Serve 之前检查，命中则关闭监听器（含 WithListener 注入实例）并返回 nil，Ready 不关闭。与 `http.Server` 自身的 inShutdown 语义互补，两层共同闭合 Stop-wins 窗口。
- **R-A**：`lynx.go` 新增 `closed` 字段（受 app.mu 保护）：Close 持锁置位并幂等（二次 Close 直接返回）；Run 入口在同一互斥域内检查，命中返回新增哨兵错误 `ErrAppClosed`（errors.go），不再执行任何钩子与服务。`lynxtest` 的 cleanup 将 `ErrAppClosed` 视为合法交错（Close 先于后台 Run goroutine 调度）不报失败。
- **R-B**：`lynx.go` addServices 的 execute 闭包在 `startWG.Done()` 之后、发布 Starting 事件之前增加 `ctx.Done()` 快速检查：中断已先行则跳过 Start 直接返回 nil（服务契约"Stop 容忍先于 Start"的对称面）。检查之后、Start 之前的残余窗口由 R-C/R-D 的 server 侧守卫兜底——实施中 D1 回归测试的第一版恰好复现了该残余窗口（仅 R-B 不足以闭合），据此确认双层守卫缺一不可。

竞态回归测试：`startup_race_test.go`（Close→Run 返回 ErrAppClosed；fail-fast 兄弟服务 ×10 循环，迷你监听服务完整镜像两层守卫——根包测试不能 import server/*，导入环）、`server/http/stopstart_test.go`（Stop→Start 快速返回 nil 不进入永久 Serve）、`server/grpc/stopstart_test.go`（Stop→Start 的 ErrServerStopped 归一化）。全仓 11 模块 `-race -shuffle=on` 通过，存量生命周期测试无语义漂移。
