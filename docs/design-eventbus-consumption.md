# Lynx EventBus 消费模型：事件订阅 + 进程内扇出

| 字段 | 内容 |
| --- | --- |
| 标题 | EventBus 消费模型修订：订阅单元从 handler 收敛为事件 |
| 作者 | TBD |
| 日期 | 2026-09-24 |
| 状态 | **Implemented**（v1.16；订阅复用/聚合确认 + 在途上限 + 组与成员数下沉后端） |
| 适用版本 | v1.16+（破坏性：删除 `WithGroup` / `WithInstances` / `WithTopicGroup` / `WithTopicInstances` / `GroupClaims` / `EffectiveGroup` / `DefaultGrouper` / `DeliveryMode`，`concurrency` 更名 `max_in_flight`） |
| 关联 | 修订 [design-eventbus.md](design-eventbus.md) §4.2（Subscribe 选项）与 §5.6（消费组冲突拦截）；索引见 [docs/README.md](README.md) |
| 相关讨论 | `WithGroup` 概念泄漏、多 handler 扇出被迫借 group 表达、claim 补偿机制膨胀、组/成员数归属后端、goroutine 防爆 |

---

## 1. 现状与问题

### 1.1 修订前模型

一次 `Subscribe` 调用 = 一个 handler = **一条 transport 订阅 = 一个消费组**（watermill `AddConsumerHandler` 每 handler 一个，`contrib/watermill/bus.go:393-411`）。同一事件多个 handler 想各收全量，只能为每个 handler 编造不同的组（`WithGroup`）。

### 1.2 问题

| ID | 问题 | 证据 |
| --- | --- | --- |
| P1 | **概念泄漏**：消费组是跨进程消费身份（部署属性），却作为通用订阅参数出现；内存后端上无意义 | `eventbus/memory.go`（忽略 + Warn）；`WithGroup` 仅对 ConsumerGroup 后端生效 |
| P2 | **扇出必须借组表达**：同事件多 handler 各收全量，在 Kafka 上必须写 `WithGroup`；不写则启动失败 | `contrib/watermill/bus.go` Claim 拒绝 |
| P3 | **违反"业务代码零差别"**：同一份 handler 代码，内存能跑、Kafka 必须加 transport 专有参数 | design-eventbus.md §1.1「低心智负担」原则 |
| P4 | **补偿机制膨胀**：`GroupClaims` / `EffectiveGroup` / `DefaultGrouper` / `DeliveryMode` 的 claim 用途 / WK-01 全套 + 已知盲区，只为拦截"两个 handler 共组"这个 API 形状自造的脚枪 | `eventbus/claim.go`、`contrib/watermill/claim_test.go`、design-eventbus.md §5.6 已知边界 |
| P5 | **"组绑事件"只是把泄漏搬家**：把组从 Subscribe 参数挪到 Topic 配置后，同一事件的 handler 仍共享一个组，第二个 handler 依然被 Claim 拒绝——组本质是后端概念（kafka `consumer.group_id`），Bus 不该建模 | 当时的 `Topic.WithTopicGroup` + Claim |

### 1.3 根因

订阅单元错了：进程内的"处理步骤"（handler）被当成了跨进程的"消费单元"（消费组）。内存后端一直是对的（广播 + 每 handler 独立 goroutine 并行）；Kafka 路径应向内存语义看齐，而不是反过来。

---

## 2. 目标设计

### 2.1 语义契约

- **订阅单元是事件（逻辑 topic），不是 handler**。同一逻辑 topic 的 N 个 handler 都收到每一条消息，**进程内并行触发**；内存与 Kafka 语义一致。
- **消费组 / 消费者成员数是后端配置**（kafka `consumer.group_id` / `consumer.instances`），Bus 不建模：Bus 只负责事件 → 订阅复用、handler 扇出与处理语义。
- **Bus 层配置**：`max_in_flight`（订阅级在途上限，默认 1）、`handler_timeout`（handler 单次尝试超时，默认不限制）与 handler 级 `auto_ack` / `continue_on_error` / `retry`；跨服务 / 跨进程的消费身份由各服务各自的后端配置承担。
- **进程内一条逻辑 topic 只建一条 transport 订阅**；不变量：同进程内同一事件只可能有一条订阅 → "共组瓜分分区"结构上不可能发生，无需运行时检查。

### 2.2 架构

```text
Subscribe(topic, handler, opts)
  │  解析 transport / key（route 配置）
  ▼
订阅注册表  key = 逻辑 topic
  ├─ 已存在 → 挂载 handler（加锁快照，下一条消息生效）
  └─ 不存在 → Transport.Subscribe（后端从自身配置取消费组 / 成员数）
               + router.AddConsumerHandler(订阅级 dispatcher)
  ▼
adapter 占用订阅级在途槽位（max_in_flight，默认 1）
  ▼
dispatcher(msg)   // 每条消息一个 goroutine（在途数受槽位约束）
  ├─ 并行调用全部 handler（各自 InvokeHandler：重试 / ctx 传播 / 日志 / panic 恢复）
  ├─ 聚合：全部"参与裁决"的 handler 成功 → Ack；任一失败 → Nack
  └─ 毒消息止损：per-handler 计数，超上限的 handler 本轮跳过并记 Error
```

落点：

| 模块 | 改动 |
| --- | --- |
| `eventbus`（核心） | 删除 `WithGroup` / `WithInstances` / `WithTopicGroup` / `WithTopicInstances` / `GroupClaims` / `EffectiveGroup` / `DefaultGrouper` / `DeliveryMode`；`SubscribeOptions.MaxInFlight` 为唯一事件级字段（配置填充，调用者不可传）；`Transport` 接口去掉 `DeliveryMode` |
| `contrib/watermill` | 订阅注册表（key = 逻辑 topic）+ 订阅级 dispatcher + 在途上限信号量；删除 claims 接线 |
| 内存 Bus | 不改（已是目标语义）；`max_in_flight` 无意义（Warn） |
| `contrib/watermill-kafka` | 消费组 / 成员数只来自自身配置（`consumer.group_id` / `consumer.instances`）；删除 `DefaultGroup` / `DeliveryMode` |

### 2.3 失败语义（共享 offset 的必然结果）

一条消息 N 个 handler 共享一个 offset，因此：

1. **Ack 条件**：全部"参与裁决"的 handler 成功（或已止损跳过）才 Ack。
2. **失败传播**：任一 handler 终态失败 → Nack → 整条重投 → **所有参与裁决的 handler 重跑**。幂等要求从"每个 handler 对自己幂等"升级为"重投会重复投给所有 handler"（文档醒目标注）。
3. **毒消息止损**：`RedeliveryLimiter` 键（handler × 消息 ID）不变；某 handler 超过 `max_redeliveries` 后在后续轮次被跳过并记 Error，其余 handler 继续；全部成功 / 跳过 → Ack（跳过者的这次处理机会被丢弃，与现状止损语义一致）。
4. **不参与裁决**：`AutoAck`（结果不影响 Ack；不再"先 Ack 后执行"，见 Q3）与 `ContinueOnError`（自身吞错）的 handler。
5. **panic**：dispatcher 为每个 handler goroutine 恢复 panic，按该 handler 终态失败处理（对齐现状 watermill Recoverer → error → Nack；注意 router 的 Recoverer 不再覆盖 dispatcher 内部的 goroutine）。
6. **并发/顺序**：消息内 handler 并行；消息间受**订阅级在途上限**约束（`max_in_flight`，默认 1，见下条）。
7. **在途上限（goroutine 防爆）**：适配器在把消息交给 router **之前**占用槽位、Ack/Nack/订阅关停时释放——在途消息 ≤ N，goroutine 上界 ≈ N×(H+2)。N=1（默认）时同订阅串行处理、恢复投递顺序；N<0 不限制（逃生口，不推荐）。限流点必须在适配器：只限 dispatcher 执行挡不住 router 的每消息 goroutine 堆积（阻塞的 goroutine 仍持有消息）。
8. **handler 超时（可选，`handler_timeout`）**：单次尝试超过上限按终态失败处理（重试 → 重投 → 毒消息止损），防止挂死的 handler 永久占用在途槽位。实现为截止 ctx + 看门狗；Go 无法终止 goroutine——handler 必须尊重 ctx，否则超时只释放调用方，goroutine 运行到自行返回（可能与被重投的尝试重叠执行）。

---

## 3. 关键决策及理由

| ID | 决策 | 备选与否决理由 |
| --- | --- | --- |
| D1 | 订阅单元 = 事件；进程内扇出 | 维持"一 handler 一订阅"：组泄漏与补偿机制长期存在，DX 不一致 → 否 |
| D2 | **消费组只存在于后端配置**（kafka `consumer.group_id`），Bus 不建模 | "组配置化到 Topic"：只是把泄漏从调用点搬到配置层，同事件两 handler 共组仍被拒 → 否 |
| D3 | 失败聚合：全成功才 Ack；毒消息 per-handler 跳过 | "任一失败即整条丢弃"：坏 handler 连坐好 handler → 否 |
| D4 | 跨服务扇出 / 竞争由配置表达；**同进程多消费身份明确不做** | 需要独立消费身份时拆服务 / 配置条目 |
| D5 | **消费者成员数只存在于后端配置**（kafka `consumer.instances`） | Bus 层副本（`bus.topics.<t>.instances`）与后端配置重复 → 删 |
| D6 | **删除 `DeliveryMode`**（Bus 不再需要投递模式） | 保留为"后端必答属性"：订阅复用键退化为逻辑 topic 后无任何调用方，死接口 → 否 |
| D7 | 订阅级在途上限命名 **`max_in_flight`**（默认 1），限流点放适配器 | `concurrency`：与 `instances` 产生"重复"错觉（Spring 里 concurrency 就是消费者线程数）→ 否；handler worker 池：H 小且静态，N×H 已有界 → 暂不做 |
| D8 | handler 超时放共享执行点 `eventbus.InvokeHandler`（截止 ctx + 看门狗），配置 `bus.handler_timeout` / `bus.topics.<t>.handler_timeout` | 放 watermill middleware：内存 Bus 拿不到，且与重试/聚合确认的交互要重复实现 → 否 |

---

## 4. 实施记录（一次性破坏性变更，Q2 拍板）

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 0 | 本文档评审，拍板 §8 开放问题 | 已完成 |
| 1 | watermill 订阅注册表 + dispatcher + 聚合确认 + 在途上限；eventbus 删除 `WithGroup` / `WithInstances` / claim 三件套 | 已实现 |
| 2 | 组 / 成员数下沉后端；删除 `DeliveryMode` 与 Topic 组选项；`concurrency` → `max_in_flight`；文档 / 示例对齐 | 已实现 |
| 3 | handler 超时（`bus.handler_timeout` / `bus.topics.<t>.handler_timeout`，`InvokeHandler` 截止 ctx + 看门狗） | 已实现 |
| 4 | 版本发布 | 待发布 |

---

## 5. 风险与对策

| ID | 风险 | 对策 |
| --- | --- | --- |
| R1 | 毒消息连坐（现状一个坏 handler 不影响好 handler） | D3 跳过策略 + Error 日志点名 handler |
| R2 | 幂等要求升级（重投重复投给成功过的 handler） | 文档醒目标注；示例注释；CHANGELOG 迁移说明 |
| R3 | 默认串行（`max_in_flight=1`）吞吐下降 | 有界优先；I/O 型 handler 按 topic 调大 `max_in_flight`；挂死 handler 由 `handler_timeout` 止损（D8） |
| R4 | 动态挂载竞态 | handler 集合加锁快照；挂载后从下一条消息生效 |
| R5 | `instances` 与 `max_in_flight` 混淆（以为 `instances` 会并行处理） | 文档明确分工：`instances` = 后端成员数（抢分区 / 连接），`max_in_flight` = 进程内在途处理上限 |
| R6 | 存量用户破坏性迁移 | §6 迁移对照 + CHANGELOG + 版本说明 |
| R7 | 部署盲区：不同逻辑 topic 路由到同一物理 topic 且组相同 | 组现在只来自 kafka 条目 → 物理 topics 重叠时必须拆成不同条目；文档警示 |
| R8 | **Kafka 提交乱序窗口**：watermill-kafka 在 Ack 时 `MarkMessage(offset+1)`（AutoCommit 周期提交或显式 `Commit`），提交高 offset 隐含提交更低 offset——`max_in_flight>1` 时高 offset 先确认、崩溃会跳过仍在处理的低 offset 消息（at-least-once 弱化）。默认 1（串行）严格按投递顺序确认，窗口关闭；旧模型无界并发下该窗口本来就存在 | 文档明示「要严格 at-least-once 保持 `max_in_flight=1`」；按分区最低未确认 offset 提交属后续项（需 transport 感知 partition） |

---

## 6. 迁移对照

| 现状用法 | 目标 |
| --- | --- |
| `WithGroup("x")` 用于同事件扇出 | 删除该参数（自动并行扇出） |
| `WithGroup("x")` 用于加入他组 | kafka `consumer.group_id` |
| `WithTopicGroup("x")` | 删除；组由 kafka `consumer.group_id` 决定 |
| `WithInstances(n)` | kafka `consumer.instances` |
| `WithTopicInstances(n)` | kafka `consumer.instances` |
| `WithTopicConcurrency(n)` | `WithTopicMaxInFlight(n)` / `bus.topics.<t>.max_in_flight` |
| `bus.topics.<t>.group` / `.instances` | kafka `consumer.group_id` / `consumer.instances` |
| 自定义 Transport 的 `DeliveryMode()` | 删除（接口不再要求） |
| claim 拒绝错误 | 不再出现（结构上不可能） |
| `lynx.NewHandlerService` 文档中的"同 topic 必须分 group" | 删除该注意事项 |

---

## 7. 测试策略

- **eventbus**：删除 claim 测试；`max_in_flight` 的配置填充与内存后端 Warn。
- **watermill**（内存 transport + 计数 fake transport）：
  - 同 topic 两个 handler 都收到每条消息且并行触发（barrier 断言）；
  - 同 topic 只调用一次 `Transport.Subscribe`（调用计数）；
  - 聚合 Ack / Nack：全成功 Ack、一失败 Nack、AutoAck / ContinueOnError 不参与；
  - 毒消息：超限 handler 被跳过、其余继续、最终 Ack；per-handler 成功清计数；
  - 在途上限：`max_in_flight=2` 峰值恰为 2；默认 1 串行且保序；负数不限；Nack 释放槽位；
  - handler 超时：挂死 handler 超时 Nack 且槽位释放（后续消息仍被处理）；超时可重试；解析优先级（调用 > 主题 > 全局，负值禁用）；
  - panic 恢复为 Nack；动态挂载；handlerName 唯一；Stop 收口。
- **watermill-kafka**：组必须来自配置（缺失报错）；`instances` 钳制；删除 `DeliveryMode` / `DefaultGroup` 测试；**testcontainers 集成测试**（`//go:build integration`）真 broker 验证单订阅扇出与配置驱动组。
- **示例**：`_examples/bus-kafka` 三路 fan-out（audit + 两 handler，无 group 参数）。
- **回归**：`_examples/bus`、lifecycle、lynxtest 全绿。

---

## 8. 开放问题（已拍板）

| ID | 问题 | 结论 |
| --- | --- | --- |
| Q1 | 失败聚合策略（D3）确认 | 采纳：全成功 Ack + per-handler 止损跳过 |
| Q2 | 一次性破坏性变更还是两阶段 | 一次性（v1.16） |
| Q3 | `AutoAck` 语义变更：不再"先 Ack 后执行"，改为"不参与确认裁决" | 采纳 |
| Q4 | 内存后端对 `max_in_flight` 的 Warn | 保留（配置错位可见） |
| Q5 | dispatcher 承载：router 订阅级 handler vs Bus 自持 goroutine | router 订阅级 handler（保留 Recoverer / CorrelationID / 动态 RunHandlers） |
| Q6 | 组归属：Bus 建模 vs 只留后端配置 | 只留后端配置（kafka `consumer.group_id`） |
| Q7 | `instances` / 处理并发：谁留 Bus 层 | `instances` 只留 kafka；Bus 层留 `max_in_flight`（原 `concurrency` 更名） |
| Q8 | `DeliveryMode` 去留 | 删除（订阅复用键退化为逻辑 topic 后无调用方） |

---

## 9. 明确不做的事

- 不做同进程内同一事件的两个独立消费身份（多组）——需要时拆服务或配置。
- 不做 per-handler offset / 独立 lag / 独立成员数。
- 不做 per-handler 并发队列（消息内 handler 并行，消息间由订阅级 `max_in_flight` 约束）。
- 不做 handler 级 worker 池（H 小且静态，N×H 已有界）。
- 不引入消息间重排序。
- 不改 `Transport.Subscribe` 签名（`opts` 保留为后端扩展缝，Bus 不填后端特有字段）。
- 不改内存 Bus 的投递实现。
- 不保留 `WithGroup` 兼容别名（Q2 拍板后）。

---

## 10. 实现对照（合入后）

| 项 | 实现 |
| --- | --- |
| 订阅注册表 + dispatcher | `contrib/watermill/subscription.go`（key = 逻辑 topic；首 handler 先入集合再注册 router handler） |
| 聚合确认 + 毒消息跳过 | `Bus.dispatch`：并行 `InvokeHandler`、全成功 Ack、任一失败 Nack、超限 handler 跳过并记 Error、per-handler 成功清计数、Ack 时清除全部计数 |
| 订阅级在途上限 | `subscriberAdapter.sem`（适配器在交给 router 前占用、Ack/Nack/关停释放）+ `bus.topics.<t>.max_in_flight` / `Topic.WithTopicMaxInFlight`，默认 1 |
| handler 超时 | `eventbus.InvokeHandler`（`invokeOnce`：截止 ctx + 看门狗）+ `bus.handler_timeout` / `bus.topics.<t>.handler_timeout` / `Topic.WithTopicHandlerTimeout`，默认不限制 |
| 组 / 成员数 | 只存在于 kafka 配置（`consumer.group_id` / `consumer.instances`）；Bus 不建模 |
| 删除 | `eventbus.WithGroup` / `WithInstances` / `WithTopicGroup` / `WithTopicInstances` / `GroupClaims` / `EffectiveGroup` / `DefaultGrouper` / `DeliveryMode`、kafka `DefaultGroup`、watermill 消费组占用接线与 claim 测试 |
| 示例 | `_examples/bus-kafka`：audit + 两个 handler 三路 fan-out，无 group 参数 |
