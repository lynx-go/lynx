# Lynx 文档索引

## 教程

| 章节 | 文件 |
| --- | --- |
| 01 介绍 | [01-introduction.md](01-introduction.md) |
| 02 快速开始 | [02-quick-start.md](02-quick-start.md) |
| 03 核心概念 | [03-core-concepts.md](03-core-concepts.md) |
| 04 服务系统 | [04-service-system.md](04-service-system.md) |
| 05 服务器 | [05-servers.md](05-servers.md) |
| 06 客户端 | [06-clients.md](06-clients.md) |
| 07 注册中心 | [07-registry.md](07-registry.md) |
| 08 测试 | [08-testing.md](08-testing.md) |

## 设计方案

| 日期 | 标题 | 状态 | 说明 |
| --- | --- | --- | --- |
| 2026-08-24 | [EventBus 一等消息总线](design-eventbus.md) | Implemented | 进程内协同与领域事件共用 Bus，Watermill Transport 扩展 |
| 2026-08-25 | [服务注册中心](design-service-registry.md) | Implemented | Registrar/Discovery/Resolver 与 consul/memory 后端 |
| 2026-09-22 | [测试套件 testkit](design-testkit.md) | Implemented | lynxtest 包 + R1-R5 可测性小幅重构，三层测试模型 |
| 2026-09-22 | [启动期竞态修复](design-startup-race.md) | Implemented | Close/interrupt 与服务 Start 交错竞态族的四点修复（closed 标志 + 双层守卫） |
| 2026-09-23 | [Resolver 订阅 API](design-resolver-subscribe.md) | Implemented | `Resolver.Subscribe` 消费侧订阅，grpcResolver 订阅驱动消除 5s 轮询（兜底 30s） |

## 评审记录

| 日期 | 文件 |
| --- | --- |
| 2026-08-25 | [review-2026-08-25.md](review-2026-08-25.md) |
