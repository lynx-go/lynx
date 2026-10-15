# registry-redis

模块路径：`github.com/lynx-go/lynx/contrib/registry-redis`

用 Redis 实现 `registry.Registry` 与 `registry.Discovery`：实例键 TTL 即租约，过期即下线；Watch 走 Pub/Sub 推送 + 兜底轮询。与 Consul 后端（`contrib/consul`）平级，Registrar / Resolver / 契约套件全部复用 `contrib/registry`。

## 存储模型

全部键携带 `key_prefix` 前缀（默认 `lynx/registry/`，多套应用共享同一 Redis 时应配不同前缀）：

| 用途 | Redis 结构 | 说明 |
| --- | --- | --- |
| 实例记录 | `{prefix}instance/{service}/{id}` = JSON(Instance)，`SET EX ttl` | TTL = `registry.heartbeat_ttl`（默认 30s）；键过期即下线，崩溃实例无需人工清理 |
| 服务索引 | `{prefix}service/{service}` = SET(实例 ID) | 读路径 `SMEMBERS` + pipeline `GET`；`GET` 为 nil 的成员惰性 `SREM` |
| 变更事件 | channel `{prefix}events/{service}` | Register/Deregister 后 `PUBLISH`；Watcher 订阅后重拉全量快照 |

## 能力要点

- `New(rdb redis.UniversalClient, opts ...Option) *Client`：注入既有连接（`*redis.Client` / Sentinel / Cluster 均可），连接归调用方；`NewFromConfig(cfg)` 自建连接并归 `Close` 托管
- `Register`：实例键 `SET EX` + 索引 `SADD` + 发事件，同一 pipeline 按序执行；同 ID 重复注册是 last-write-wins upsert（Consul 同语义）
- `Heartbeat`：Lua `PEXPIRE` 续命；键已过期返回 `registry.ErrNotRegistered`（与 Consul TTL check 失败同语义，Registrar 的心跳循环只续命不重注册）
- `Deregister`：`DEL` + `SREM` + 发事件，天然幂等
- `GetService`：索引 + pipeline `GET`，`registry.MatchFilter` 后置过滤（与 memory/DNS/Consul 返回形状一致）
- `Watch`：订阅服务频道，消息到达即重拉快照提交；另按 `watch_fallback_interval`（默认 15s，对齐 Resolver 轮询回退节奏）兜底重拉——覆盖 Pub/Sub 两个盲区：**键静默过期不发事件**、订阅断线丢消息。过滤/规范相等/首快照语义由 `registry.WatcherSession` 承载
- `deregister_after` 在本后端无意义（TTL 过期即注销），字段被忽略

## 快速开始

```go
import (
    "github.com/lynx-go/lynx"
    "github.com/lynx-go/lynx/contrib/registry"
    registryredis "github.com/lynx-go/lynx/contrib/registry-redis"
)
```

```yaml
registry:
  enabled: true
  backend: redis
  heartbeat_interval: 10s   # Registrar 心跳间隔（须 < heartbeat_ttl）
  heartbeat_ttl: 30s        # 实例键 TTL
  redis:
    address: 127.0.0.1:6379
    # username / password / db / pool_size / min_idle_conns
    # dial_timeout / read_timeout
    key_prefix: lynx/registry/   # 多应用共库时区分命名空间
    # tls: { enabled, ca_file, cert_file, key_file, insecure_skip_verify }
```

```go
// setup（NewFromConfig 自建连接，Close 一并关闭）：
c, err := registryredis.NewFromConfig(app.Config())
if err != nil {
    return err
}

// Registrar：注册本进程；排水注销钩子由 Register 登记时自动挂载。
reg, err := registry.NewFromConfig(app.Config(), c, registry.HTTP(hs, registry.ProtocolHTTP))
if err != nil {
    return err
}
if reg != nil {
    app.Register(reg)
}

// Resolver：客户端侧缓存 + watch 发现。
rslv := registry.NewResolver(c)
```

复用既有连接（Sentinel / Cluster / 连接池共享）时改用 `New(rdb, ...)`，`Close` 不动调用方的连接（`cluster-redis` 的「谁构造谁负责」约定）。

## 与 Consul 后端的取舍

- 已有 Redis、不想引入 Consul 的团队：本模块零额外组件；代价是目录强依赖 Redis 可用性（Redis 抖动 = 目录抖动），且无健康探针语义（只有 TTL 租约，实例「活着但不健康」不会摘流——`Status` 由注册方写入，不经服务端检查）
- 需要 HTTP/gRPC 主动健康检查、`DeregisterCriticalServiceAfter`、多数据中心目录时用 Consul

## 相关文档与示例

- [contrib/registry](../registry/README.md) — Registry/Discovery 端口、Registrar/Resolver、契约套件
- [contrib/consul](../consul/README.md) — Consul 后端（Session + TTL check + blocking query）
- [contrib/cluster-redis](../cluster-redis/README.md) — Redis 版 `cluster.Coordinator`（同库不同命名空间，互不冲突）
- [_examples/registry-redis](../../_examples/registry-redis/) — 跨进程完整示例（本进程注册 + peer 写入 + `discovery://` 调用，含 redis-cli 观察目录的方法）
- 包内测试：`conformance_test.go`（契约套件）、`redis_test.go`（TTL/推送/兜底/惰性清理，miniredis）、`integration_test.go`（真实 Redis，`-tags integration`）
