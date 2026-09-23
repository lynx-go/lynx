# cluster

模块路径：`github.com/lynx-go/lynx/contrib/cluster`

进程间协调（跨进程互斥、选主、单实例运行）的纯接口与配方包：定义 `Coordinator` 端口与 `TryOnce` / `Campaign` / `Singleton` 三个上层配方，不含任何后端依赖。生产适配器在 [contrib/consul](../consul/README.md)（Consul KV + Session）与 [contrib/cluster-redis](../cluster-redis/README.md)（Redis SET NX）。

## 能力要点

- `Coordinator` 端口（`coordinator.go:31`）：`Claim` 一次性占位（ttl 后自动过期，无续约、不释放）；`Acquire` 长租约（持有期间按 ttl 续约，崩溃后约在 ttl 内过期）。`won=false` / `ok=false` 且 `err=nil` 表示被占用（跳过，不是错误）
- `Lease`（`coordinator.go:41`）：`Context()` 在续约失败、租约丢失或 `Release` 后取消；`Release` 幂等
- 命名选项：`WithNamespace(ns)`（默认 `"lynx"`，实际键 `{ns}/{name}`，`coordinator.go:75`）、`WithInstance(id)`（占位 value，仅供排障）；`FormatKey` / `Owner` 导出键与 owner 供适配器使用（`coordinator.go:95`）
- `TryOnce(ctx, s, name, ttl, fn)`（`once.go:10`）：抢到才执行 fn 且不释放，靠 ttl 过期；未抢到返回 `skipped=true`——多副本任务只跑一份
- `Campaign` / `CampaignTTL`（`campaign.go:25`）：循环 Acquire 直到当选，返回 `Leadership`（`Context()` / `Resign()`）；失败重试间隔 50ms，默认租约 `DefaultLeaseTTL = 15s`
- `Singleton(name, inner, s)`（`singleton.go:27`）：把任意 `lynx.Service` 包装成"只有 leader 才 `Start`"的 Service——当选运行 inner，失联（租约丢失）自动停 inner 并重新竞选，`Stop` 时 Resign；实现 `lynx.Checker`，未当选时健康恒过
- `NewMemory(opts...)`（`memory.go:26`）：进程内 Coordinator，供单测与单进程，不能跨进程

## 快速开始

```go
import "github.com/lynx-go/lynx/contrib/cluster"

coord := cluster.NewMemory() // 测试；生产用 consul / cluster-redis 适配器

// 1) 多副本任务只跑一份（一次性占位，ttl 过期前互斥）
skipped, err := cluster.TryOnce(ctx, coord, "migration/v42", time.Hour, func(ctx context.Context) error {
	return runMigration(ctx)
})
if skipped { /* 已被其它副本执行 */ }

// 2) 选主：成为 leader 才持有角色，Resign 让位
lead, err := cluster.Campaign(ctx, coord, "worker/leader")
defer lead.Resign(context.Background())
select {
case <-lead.Context().Done(): // 租约丢失，不再是 leader
case <-done:
}

// 3) 单实例服务：整个 Service 只在 leader 节点运行
app.Register(cluster.Singleton("report", reportService, coord))
```

## 与 lynx 核心的集成

- `Singleton` 返回值同时实现 `lynx.Service` 与 `lynx.Checker`（`singleton.go:120`），直接 `app.Register`；`Init` 先校验 `inner` / Coordinator / name 非空再透传 `inner.Init`
- leader 副本中 `Singleton.CheckHealth` 透传 inner 的 `CheckHealth`（inner 实现时），follower 恒返回 nil——未当选不会拖红整个进程
- `TryOnce` / `Campaign` 是普通函数，可放 `OnPreStart` 钩子或业务代码中，不接管生命周期
- Coordinator 适配器的连接由调用方管理：consul 的 `Client` 随 Registrar 关闭，Redis 客户端由业务自建（见各适配器 README）

## 相关文档与示例

- [contrib/consul](../consul/README.md) — `Client.Coordinator()`（KV + Session，Session TTL 下限 10s）
- [contrib/cluster-redis](../cluster-redis/README.md) — Redis SET NX 实现（支持短 TTL）
- 包内测试 `cluster_test.go` 覆盖互斥、过期、续约、让位与 Singleton 单 leader 语义
