# Resolver 订阅 API 设计方案

| 字段 | 内容 |
| --- | --- |
| 标题 | Resolver 消费侧订阅 API（`Subscribe`）：消除 gRPC 发现轮询 |
| 作者 | TBD |
| 日期 | 2026-09-23 |
| 状态 | **Implemented**（Subscribe + grpcResolver 订阅驱动已落地；兜底轮询保留 30s；设计文档先于实现定稿，实现期补 Stop 广播唤醒细节） |
| 适用版本 | v1.13+ |
| 相关讨论 | ROADMAP G5（Phase F 遗留）、`grpc_resolver.go` 轮询缺口注释、RC-04 watcher 泄漏教训、`grpcDefaultPollInterval` 不暴露为 Option 的权宜 |

---

## 一、现状与问题

- **后端订阅已存在**：`Discovery` 接口已有 `Watch(ctx, name, filter) (Watcher, error)`，Memory（即时推送）与 Consul（blocking query，含 index 回绕回归测试）均已实现；DNS 后端不支持（返回 err）。`Resolver` 缓存层每服务名一个 `watchLoop` goroutine，Watch 推送优先、失败回退 15s 轮询——缓存本身已是订阅驱动的。
- **缺口在消费侧**：`Resolver` 对外只有拉式 `Get`/`GetAll`（读缓存快照），没有"缓存变了通知我"的 API。于是 `grpcResolver` 只能 5 秒轮询缓存翻译成 `UpdateState`——实例增删最多延迟一个轮询周期才反映到连接地址。

## 二、目标设计

### 2.1 核心 API（`Resolver` 新增，纯增量）

```go
// Subscribe 订阅服务实例集变化（缓存层订阅）：后端 Watch 推送/轮询
// 兜底已由 Resolver 内部维护，订阅者对后端形态无感（DNS 后端下
// 缓存由轮询维护，同样触发通知）。
// 契约与 Discovery.Watcher 同构：
//   - 缓存已填充时首个 Next 立即返回当前快照；未填充则等待首次 store；
//   - 后续 Next 返回最近一次快照变化，慢消费者不排队陈旧快照
//     （推送缓冲 1 最新替换，总是最新）；
//   - 快照已按 filter 过滤（MatchFilter 语义，与 Watch/GetAll 读路径
//     一致）；每个服务名共享一条缓存与推送源，过滤在订阅边界应用；
//   - 返回切片只读（不深拷贝，与后端 Watcher.Next 一致）；
//   - Stop 退订且幂等；Stop 后 Next 返回 ErrWatcherStopped，
//     Resolver.Close 后返回 ErrResolverClosed。
func (r *Resolver) Subscribe(name string, filter Filter) (Watcher, error)
```

校验：空 name → `ErrBadName`；`Resolver` 已关闭 → `ErrResolverClosed`；未查询过的 name 经 `entryFor` 惰性创建（与 `Get` 行为一致，触发 watchLoop）。

### 2.2 内部机制（`cacheEntry` 扩展）

- `cacheEntry` 增加 `subs map[uint64]*subscription` 与自增 ID；
- **触发点统一为 `store()`**：watchLoop 推送、轮询回退、`ensureFilled` 同步首填全部经 store，天然全覆盖；store 末尾对每个订阅应用其 Filter 后 `Push`（缓冲 1 最新替换 = 慢消费者不排队陈旧快照）。无订阅者时空 map 判断零成本；
- **首推**：`addSub` 时若 `filled`，向新订阅者预推一份当前过滤快照（首个 Next 立即返回）；未填充则等待首次 store；
- **骨架**：订阅与三个后端 watcher 共用 `registry.WatcherCore[T]`（首次语义注入、Push 合并、Stop 幂等 + 注销钩子）；停止/取消优先于挂起推送；
- **stale 丢弃不通知**：与 grpcResolver"解析出错保留上次状态"的既有惯例一致，兜底轮询覆盖；
- 哨兵统一为 `ErrWatcherStopped`（后端与订阅同词；此前 registry/consul 各持私有副本）。

### 2.3 grpcResolver 改造（订阅驱动 + 兜底轮询）

- loop 增加订阅消费：`Subscribe(name, filter)` 的 `Next()` 返回即走既有翻译路径（Protocol 过滤 → `resolver.Address` → 排序比较去重 → `UpdateState`）。去重、空快照立即生效、出错保态三条语义原样保留；
- 兜底轮询保留：`grpcDefaultPollInterval`（5s）改名 `grpcFallbackPollInterval` 并放宽为 **30s**——正常路径零轮询，异常时 30s 内自愈；`ResolveNow` 保留（直接走既有 `GetAll` 路径）；
- 订阅 `Next` 返回 `ErrWatcherStopped`/`ErrResolverClosed` 时退出订阅 goroutine，主循环退回纯兜底轮询（不退出——gRPC 连接可能仍由 lastState 服务）。

## 三、关键决策及理由

1. **`Subscribe` 返回 `Watcher`（复用接口）而非回调/channel**——回调需定义并发调用与 panic 归属；裸信号 chan 不通用。与 `Discovery.Watcher` 同构让消费方代码形态统一，阻塞 Next + Stop 的生命周期语义现成。否掉了 `grpc_resolver.go` 注释里提的 "OnChange 回调"形态。
2. **快照而非增量 diff**——消费方合并需求各异，diff 责任留在消费方（排序比较去重已存在）；Resolver 侧保持每 name 一条缓存与推送源的简单结构。**修订（骨架收敛批次）**：过滤下沉到订阅边界——`Subscribe(name, filter)` 返回已过 MatchFilter 的快照，与 Watch/GetAll 读路径一致；缓存仍按服务名共享一条，消费方不再自行补过滤。
3. **信号合并不排队**——服务发现订阅的标准语义：慢消费者只关心最新集合，陈旧快照队列毫无价值。
4. **兜底轮询保留但放宽至 30s**——完全去掉轮询会让订阅链路故障不可自愈；正常路径订阅已在毫秒级推送，30s 兜底无常态成本。
5. **不加 filter 参数**——缓存以 name 为单位全量存储（`filterAll` 的既有设计），过滤在读路径应用；订阅通道同构共享。

## 四、分步实施计划

1. **订阅机制**：`ErrWatcherStopped` + `cacheEntry.subs` + `store()` 通知 + `Subscribe` + 单测（独立可交付，grpc 仍轮询）；
2. **grpcResolver 订阅驱动**：loop 改造 + fallback 放宽 + 单测；
3. **文档同步**：registry README、CLAUDE.md、ROADMAP G5 勾选（发版时）。

每步验证：`go test -race ./...`（registry 模块）+ consul 集成回归。

## 五、风险与对策

- **store 热路径 fan-out 开销**：空 map 判断 + N 次非阻塞 send，微秒级；订阅者通常 1-2 个（每 gRPC 连接一个）；
- **信号合并漏中间态**：收敛语义，接受；
- **订阅 goroutine 泄漏**：`grpcResolver.Close` 必须调 `sub.Stop()`（RC-04 教训——任何退出路径都 Stop）；
- **回滚**：API 纯增量 + grpcResolver 内部改造，revert 单 commit 即回滚到纯轮询，外部行为不破坏。

## 六、测试策略

- 单测（fake Discovery 手动推送）：首推/信号合并/stale 不通知/Stop 后 `ErrWatcherClosed`/Close 后 `ErrResolverClosed`/DNS 式后端（Watch err）下订阅仍可用；
- grpc 单测（mock ClientConn）：实例变化一个信号内 `UpdateState`、同集不重复推送、订阅停止后退回兜底轮询；
- consul 集成：blocking query 推送 → Subscribe 通知的端到端链路（注册新实例秒级收到，而非等轮询）。

## 七、明确不做

- HTTP transport 订阅化（每请求读缓存已实时，无轮询问题）；
- 增量 diff 推送；跨 Resolver 的总线桥接；DNS 后端实现 Watch（协议限制，轮询回退已覆盖）。
