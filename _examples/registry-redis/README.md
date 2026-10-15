# registry-redis 示例

用 `contrib/registry-redis` 后端演示服务注册与发现的最小闭环：HTTP 服务
把实例注册进 Redis（TTL 即租约）→ 客户端用 `discovery://` URI 按服务名
调用。与 `../registry/`（memory 后端，进程内目录）相对，本示例是**跨进程
目录**：多个进程指向同一 Redis 即可互相发现。

## 运行

需要可达的 Redis（默认 `127.0.0.1:6379`，可用 `LYNX_REGISTRY_REDIS_ADDRESS`
覆盖）。没有现成实例时：

```bash
docker run --rm -p 6379:6379 redis:7-alpine
```

```bash
go run .
# 或指定监听地址与 Redis 地址
go run . --addr=127.0.0.1:9090
LYNX_REGISTRY_REDIS_ADDRESS=redis.internal:6379 go run .
```

启动约 3 秒后可在日志中看到两次 `discovery:// call ok`：

- `registry-redis-demo`：本进程 Registrar 注册的服务（`config.yaml` 的
  `service.name`）；
- `registry-redis-peer`：演示代码手动写入目录的「另一个服务」，指向同一
  端口，展示目录内多服务共存与按名寻址。

用 `redis-cli` 可直接观察目录（键前缀 `lynx/registry/`）：

```bash
redis-cli smembers lynx/registry/service/registry-redis-demo   # 索引：实例 ID
redis-cli ttl lynx/registry/instance/registry-redis-demo/<id>  # 租约剩余秒数，
                                                               # 每次心跳回到 30
```

Ctrl-C 停止：排水钩子先注销实例再关监听；直接 `kill -9` 则实例键在
`heartbeat_ttl`（30s）后自动过期——这是 TTL 后端对崩溃进程的自愈方式。

## 关键代码点

- `main.go` `registryredis.NewFromConfig(app.Config())`：读 `registry:`
  段共享字段（`backend`/`heartbeat_interval`/`heartbeat_ttl`）与
  `registry.redis.*` 子树，自建 Redis 连接并归 `Client.Close` 托管；
  `backend` 非 redis 时返回 `(nil, nil)`，回落
  `registry.NewBackendFromConfig`（memory/dns）。
- `main.go` `registry.NewFromConfig(cfg, wr, registry.HTTP(hs, ...))` +
  `app.Register(reg)`：Registrar 侧与 memory/consul 后端完全同构——后端
  差异被 `registry.Registry` 接口隔离，业务代码零改动；排水注销钩子由
  框架登记时自动挂载（`lynx.Drainer`）。
- TTL 语义：`Register` 写实例键 `SET EX heartbeat_ttl`，Registrar 心跳
  循环每 `heartbeat_interval` 调 `Heartbeat`（Lua `PEXPIRE` 续命）；
  键过期后 `Heartbeat` 返回 `ErrNotRegistered`（readiness 随之失败）。
- `main.go` `registry.NewResolver(disc)` + `NewHTTPTransport(rslv).Wrap(...)`：
  客户端发现。Redis 后端的 Watch 走 Pub/Sub 推送 + 兜底轮询，注册/注销
  毫秒级反映到本进程缓存；`discovery://<service>/<path>` 由 Transport
  改写为具体实例地址，每次请求（含重试）重新解析选实例。
- 复用既有连接（Sentinel / Cluster / 连接池共享）时不走 `NewFromConfig`，
  改用 `registryredis.New(rdb, ...)` 注入，`Close` 不动调用方的连接
  （见 `contrib/registry-redis/README.md`）。
