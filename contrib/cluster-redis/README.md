# cluster-redis

模块路径：`github.com/lynx-go/lynx/contrib/cluster-redis`

用 Redis 实现 `cluster.Coordinator`：`SET NX` 抢占 + Lua 比对 token 续约 / 删除。这是**协调后端，不是给业务用的 Redis 客户端**（`coordinator.go:1` 包注释）——只做跨进程互斥与选主的键空间操作，不代理任何业务读写。

## 能力要点

- `NewCoordinator(rdb redis.Cmdable, opts ...cluster.Option) cluster.Coordinator`（`coordinator.go:26`）：`rdb` 通常是 `*redis.Client`，连接由调用方创建与关闭，本模块不托管
- `Claim`：`SetNX(key, owner, ttl)`，占位靠 ttl 自动过期、无续约不释放；value 为 `cluster.Owner(opts...)`（`WithInstance` 设置的排障标识，默认空）
- `Acquire`：同样 `SET NX`，但 value 是随机 16 字节 hex token；租约 goroutine 按 `ttl/3` 周期用 Lua `GET == token 则 PEXPIRE` 续约，续约失败（key 已易主 / Redis 不可达）即取消 `Lease.Context()`
- `Release`：Lua `GET == token 则 DEL`——token 比对防止误删他人已接管的 key；幂等
- 键名与命名空间沿用 cluster 包：实际键 `{namespace}/{name}`（默认 `lynx/`，经 `cluster.WithNamespace` / `cluster.WithInstance` 配置）
- 输入校验与 cluster 接口契约一致：空 name 返回 `cluster.ErrEmptyName`，`ttl <= 0` 返回 `cluster.ErrInvalidTTL`
- 与 Consul 实现（Session TTL 下限 10s）不同，本实现无 TTL 下限，适合秒级短间隔的互斥

## 快速开始

```go
import (
	"github.com/lynx-go/lynx/contrib/cluster"
	clusterredis "github.com/lynx-go/lynx/contrib/cluster-redis"
	"github.com/redis/go-redis/v9"
)

rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
defer rdb.Close() // 连接生命周期归业务

coord := clusterredis.NewCoordinator(rdb,
	cluster.WithNamespace("my-app"),
	cluster.WithInstance(hostname), // 写进 key 的排障 value
)

// 配方一：多副本任务只跑一份（ttl 过期前互斥）
skipped, err := cluster.TryOnce(ctx, coord, "cleanup/tmp", 10*time.Minute, func(ctx context.Context) error {
	return cleanup(ctx)
})

// 配方二：单实例 Service，只在 leader 节点运行
app.Register(cluster.Singleton("report", reportService, coord))

// 配方三：直接持有长租约
lease, ok, err := coord.Acquire(ctx, "worker/leader", 15*time.Second)
if ok {
	defer lease.Release(context.Background())
	select {
	case <-lease.Context().Done(): // 续约失败，租约丢失
	case <-done:
	}
}
```

## 与 lynx 核心的集成

- 本模块只产出一个 `cluster.Coordinator`，不实现 `lynx.Service`；接入点在 cluster 配方——`cluster.Singleton(...)` 返回的 Service 直接 `app.Register`
- Redis 客户端由应用自建（Wire provider 或 setup 函数），在 `OnPostStop` 等收尾钩子里关闭；本模块不注册任何服务或钩子
- 续约 goroutine 随 `Release` / 租约丢失自动退出，无需显式停止

## 相关文档与示例

- [contrib/cluster](../cluster/README.md) — `Coordinator` 端口、`TryOnce` / `Campaign` / `Singleton` 配方
- [contrib/consul](../consul/README.md) — Consul KV + Session 实现（TTL 下限 10s 的场景）
- 包内测试 `coordinator_test.go`（miniredis）覆盖互斥、过期、续约与非法输入
