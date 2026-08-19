# 7. 服务注册与发现

`contrib/registry` 提供可选的服务注册（写目录）与发现（读目录）能力，
`contrib/consul` 提供生产级 Consul 后端。两者都是**可选 contrib**：
不引入它们时框架行为与此前完全一致，零成本、零依赖。设计与失败模式的
完整论证见 [设计文档](./design-service-registry.md)，本章是使用教程。

- [7.1 定位与适用场景](#71-定位与适用场景)
- [7.2 架构：Registrar / Resolver / 后端](#72-架构registrar--resolver--后端)
- [7.3 快速上手（memory 后端）](#73-快速上手memory-后端)
- [7.4 配置参考](#74-配置参考)
- [7.5 生产后端：Consul](#75-生产后端consul)
- [7.6 宣告地址](#76-宣告地址)
- [7.7 排水与注销时序](#77-排水与注销时序)
- [7.8 健康模型：三条通道](#78-健康模型三条通道)
- [7.9 客户端发现（registry://）](#79-客户端发现registry)
- [7.10 DNS 后端与 Headless Service](#710-dns-后端与-headless-service)
- [7.11 失败模式摘要](#711-失败模式摘要)
- [7.12 Wire 集成](#712-wire-集成)
- [7.13 CLI 约定](#713-cli-约定)

## 7.1 定位与适用场景

服务注册发现解决的是「没有基础设施 LB 时，客户端如何找到实例」的问题。
按环境选择：

- **K8s（ClusterIP Service）**：kube-proxy 已经在做负载均衡，直接拨
  `http://user-service` 即可，**不需要**本模块。排水摘流靠核心的
  `WithDrainTimeout`（readiness 先失败 → endpoint 摘除 → 再关监听，
  见[第 3 章](./03-core-concepts.md) 3.7 节）。
- **K8s（Headless Service）或多端口直连场景**：可用本模块的 DNS 后端
  （见 7.10），Picker 才有意义。
- **裸机 / 虚拟机 / 跨集群**：用 `contrib/consul` 后端，配合
  `OnDrain` 排水注销获得秒级摘流。

「K8s ClusterIP + DrainTimeout」与「裸机 Consul + 注册发现」是两条并列
的推荐路径，不要混用两层 LB（注册中心 Picker 与 kube-proxy 同时选实例
只会让故障定位更困难）。

## 7.2 架构：Registrar / Resolver / 后端

三个角色，全部由接口解耦：

| 角色 | 类型 | 职责 |
| --- | --- | --- |
| 后端 | `Registry`（写）/ `Discovery`（读）接口 | 与目录交互：memory、DNS、Consul |
| Registrar | `*registry.Registrar`（`lynx.Service` + `lynx.Checker`） | 进程级生命周期：Start 注册 + 心跳，Stop/排水注销 |
| Resolver | `*registry.Resolver` | 客户端侧缓存 + Watch + Picker 选实例 |

数据模型：**一个进程一条 `Instance`，可挂多个 `Endpoint`**（一进程
一个 HTTP + 一个 gRPC = 一条 Instance、两个 Endpoint）。`Instance` 的
Name/ID/Version 默认取 `lynx.Meta`（即核心配置键 `service.name` /
`service.id` / `service.version`），可用 `registry.service_name` /
`registry.instance_id` 覆盖。

Registrar 遵循既有生命周期契约：`Init` 只读 `AppContext` 解析身份与
宣告地址（不注册钩子）；`Start` 注册并维持心跳，随后阻塞；`Stop` 幂等
注销。它不是对外服务：心跳连续失败只影响 readiness，永远不影响
liveness。

## 7.3 快速上手（memory 后端）

memory 后端是进程内 `Registry` + `Discovery`，用于测试、单进程场景与
本地联调。最小可运行示例（完整版见
[`_examples/registry`](../_examples/registry/)）：

```yaml
# config.yaml
addr: "127.0.0.1:8080"
registry:
  backend: memory
```

```go
runner := lynx.NewRunner(func(app lynx.App) error {
    hs := http.NewServer(router,
        http.WithAddr(app.Config().GetString("addr")),
        http.WithHealthCheckers(app.HealthCheckers),
    )

    // 后端：memory 同时作为 Registry 与 Discovery
    wr, disc, err := registry.NewBackendFromConfig(app.Config())
    if err != nil {
        return err
    }
    // Registrar：registry.HTTP 把 HTTP 服务器包装为 Advertiser，
    // 注册前最多等待 advertise_timeout（默认 5s）直到 Addr() 非空
    reg, err := registry.NewFromConfig(app.Config(), wr,
        registry.HTTP(hs, registry.ProtocolHTTP),
    )
    if err != nil {
        return err
    }
    // Bind = app.Register(reg) + 挂 OnDrain 注销钩子；reg 为 nil 时 no-op
    registry.Bind(app, reg)
    app.Register(hs)

    // 客户端：Resolver + registry:// Transport
    rslv := registry.NewResolver(disc)
    cli := clienthttp.New(clienthttp.WithClientOptions(func(c *gohttp.Client) {
        c.Transport = registry.NewHTTPTransport(rslv).Wrap(c.Transport)
    }))
    // 之后即可用服务名寻址：cli.Get(ctx, "registry://my-app/hello")
    _ = cli
    return nil
})
```

要点：

- `registry` 段缺失、`enabled: false` 或 `backend: ""` 时，
  `NewBackendFromConfig` / `NewFromConfig` 返回 nil——**未启用即零
  开销**，`Bind(app, nil)` 是 no-op，同一套代码可在启用/未启用两种
  环境运行。
- Registrar 的注册发生在 `Start`（所有服务并发启动之后），并通过
  Advertiser 等待真实监听地址，因此**不要**在 `OnStart` 里手动注册。

## 7.4 配置参考

`registry.NewBackendFromConfig` / `registry.NewFromConfig` /
`consul.NewFromConfig` 各自 `UnmarshalKey` 自己关心的子树：

```yaml
registry:
  enabled: true
  backend: consul        # memory | dns 由 registry.NewBackendFromConfig 构造
                         # consul 由应用调用 consul.NewFromConfig（见 7.5）
  fail_fast: true        # 首次注册失败时 Start 是否返回错误（默认 true）
  affect_readiness: true # Registrar 健康状态是否参与 readiness 聚合（默认 true）
  heartbeat_interval: 10s
  heartbeat_ttl: 30s     # 供 TTL 后端（consul）使用，Registrar 本身不读
  deregister_after: 60s  # 同上（Consul DeregisterCriticalServiceAfter）
  advertise_timeout: 5s  # 等待 Advertiser 出现非空 Endpoints 的上限
  tags: ["api", "internal"]
  meta:
    region: cn-east
  weight: 100            # 写入目录；v1 内置 Picker 忽略
  service_name: ""       # 覆盖 service.name
  instance_id: ""        # 覆盖 service.id（默认 hostname）
  advertise:
    host: ""             # 空则读 LYNX_ADVERTISE_HOST；再空且 endpoint 无 host → Init 失败
  endpoints:             # 静态 endpoint（生产推荐，K8s 用 Downward API 填 host）
    - protocol: http
      address: ":8080"   # 裸端口由 advertise host 在 Init 补全
    - protocol: grpc
      address: ":9090"
  health_check:          # Consul check（由 contrib/consul 读取）
    type: http           # 有 HTTP 口时必须用 http（打 /healthz/readiness）
                         # gRPC-only：ttl（禁止只靠 grpc check，见 7.8）
    path: /healthz/readiness
    interval: 10s
    timeout: 3s
  discovery:
    poll_interval: 15s   # Watch 不可用时的轮询间隔
  consul:                # 由 consul.NewFromConfig 读取
    address: 127.0.0.1:8500
    token: ""            # 空则 contrib/consul 直读 CONSUL_HTTP_TOKEN
    datacenter: ""
    namespace: ""
    allow_stale: false   # Watch/Get 默认 consistent
    tls:
      enabled: false
      ca_file: ""
      cert_file: ""
      key_file: ""
      insecure_skip_verify: false
  dns:                   # backend: dns 时生效（见 7.10）
    domain: "svc.cluster.local"
    namespace: default   # 查询名：{name}.{namespace}.{domain}
    ports:               # A/AAAA 无 SRV 时按协议补端口
      http: 8080
      https: 8443
      grpc: 9090
```

环境变量**不会**因默认 flags 自动生效。生效路径：

| 键 | 如何到达进程 |
| --- | --- |
| `registry.advertise.host` | 配置文件，或 Registrar 直读 `LYNX_ADVERTISE_HOST` |
| `registry.consul.token` | 配置文件，或 `contrib/consul` 直读 `CONSUL_HTTP_TOKEN` |
| 其它 `registry.*` | 仅当应用自己的 `WithBindConfigFunc` 写了 `BindEnv` |

框架**不新增默认 CLI flag**——发现是可选 contrib，不应出现在默认
flag 集合里。需要环境变量覆盖时在自己的 `BindConfigFunc` 中绑定：

```go
lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
    c.SetEnvPrefix("LYNX")
    c.AutomaticEnv()
    _ = c.BindEnv("registry.enabled", "LYNX_REGISTRY_ENABLED")
    _ = c.BindEnv("registry.backend", "LYNX_REGISTRY_BACKEND")
    _ = c.BindEnv("registry.consul.address", "LYNX_REGISTRY_CONSUL_ADDRESS")
    return nil
})
```

## 7.5 生产后端：Consul

`contrib/consul` 的 `Client` 同时实现 `Registry` 与 `Discovery`。
**由应用侧构造**，不是 `registry.NewBackendFromConfig`（后者只建
memory/dns，遇到 `backend: consul` 会明确报错，避免 contrib 依赖环）：

```go
runner := lynx.NewRunner(func(app lynx.App) error {
    hs := http.NewServer(mux,
        http.WithAddr(app.Config().GetString("addr")),
        http.WithHealthCheckers(app.HealthCheckers),
    )
    gs := grpc.NewServer(
        grpc.WithAddr(":9090"),
        grpc.WithHealthCheckers(app.HealthCheckers),
    )

    var wr registry.Registry
    var disc registry.Discovery
    switch app.Config().GetString("registry.backend") {
    case "consul":
        c, err := consul.NewFromConfig(app.Config()) // registry 关闭时返回 (nil, nil)
        if err != nil {
            return err
        }
        if c != nil {
            wr, disc = c, c
        }
    default: // memory / dns / 空
        var err error
        wr, disc, err = registry.NewBackendFromConfig(app.Config())
        if err != nil {
            return err
        }
    }
    if reg, err := registry.NewFromConfig(app.Config(), wr,
        registry.HTTP(hs, registry.ProtocolHTTP),
        registry.GRPC(gs),
    ); err != nil {
        return err
    } else {
        registry.Bind(app, reg) // wr==nil 时 NewFromConfig 返回 nil；Bind no-op
    }
    _ = disc // 供 Resolver 使用（见 7.9）

    app.Register(hs, gs)
    return nil
}, lynx.WithDrainTimeout(15*time.Second))
```

Consul 侧要点：

- 注册走 `Agent.ServiceRegister`，主端口取第一个匹配 check 协议的
  Endpoint；其余 Endpoint 写入 Meta 键 `lynx_endpoints`，由 consul 模块
  在读路径还原（Resolver / memory / DNS 从不解析该键）。
- Check 类型见 7.8 的选择表；Watch 是 blocking query，默认
  `allow_stale: false`（consistent 读）。
- Token 不进日志；配置为空时直读官方环境变量 `CONSUL_HTTP_TOKEN`。

## 7.6 宣告地址

注册到目录的地址必须是 `host:port`（禁止裸 `:8080`）。来源按优先级：

1. `registry.endpoints` 静态配置（生产推荐）中已带 host 的地址；
2. `registry.advertise.host` 配置，用于补全裸端口 endpoint；
3. 环境变量 `LYNX_ADVERTISE_HOST`（Registrar 直读，不经 Viper）；
4. Advertiser：`registry.HTTP(hs, ...)` / `registry.GRPC(gs)` 读
   服务器的 `AdvertiseAddr()`（`WithAdvertiseAddr` 显式设置时优先），
   否则回落 `Addr()`（Start 后的真实监听地址）。

裸端口 endpoint 且没有任何 advertise host 来源时，`Init` 直接失败——
框架**不做**「第一块非回环网卡」之类的猜测。

K8s 中用 Downward API 注入 Pod IP：

```yaml
env:
  - name: LYNX_ADVERTISE_HOST
    valueFrom:
      fieldRef:
        fieldPath: status.podIP
```

多实例共存靠不同的 `service.id`（容器里 hostname 默认已唯一）；需要更
稳的 ID 时同样用 Downward API 注入 `metadata.name`。debug/pprof 等内部
端口**不要**宣告进目录。

## 7.7 排水与注销时序

排水（Drain）是核心 v1.1 的既有能力（`WithDrainTimeout`，见
[第 3 章](./03-core-concepts.md) 3.7 节）；注册发现只是给它加了一个
标准消费者：`registry.Bind` 会把 `Registrar.DeregisterHook()` 挂到
`app.OnDrain`——**排水置位那一刻**就从目录删除实例（delete 语义，
不存在「draining 中间态」），Watch 立即推空，客户端在 RTT 级时间内
停止拨入，而不是等 `DrainTimeout` 窗口结束。

时序：

1. 关停信号 → `SetDraining(true)`：readiness 聚合立即失败，
   检查器返回导出的 `lynx.ErrDraining`；
2. **并发**两段预算：排水睡眠 `DrainTimeout` 与 OnDrain 钩子
   （总预算 `DrainHookTimeout`，默认 3s，`WithDrainHookTimeout`
   调整）；钩子错误/超时记入 `ShutdownErrors` 并继续，不打断排水；
3. 窗口结束 → cancel ctx → OnStop → 各服务 Stop（Registrar.Stop 幂等，
   已注销则只关闭 Registry）。

关停时长上界（`terminationGracePeriodSeconds` 必须覆盖）：

| 情况 | 上界 |
| --- | --- |
| 无 `OnDrain` 钩子 | `DrainTimeout + ShutdownTimeout + Σ StopTimeout` |
| 有钩子 | `max(DrainTimeout, DrainHookTimeout) + ShutdownTimeout + Σ StopTimeout` |

注意 `DrainTimeout=0` 且注册了钩子时，上界比纯 v1.1 行为多出最多
3s——这是有意的：注销从排水置位就开始，通常在 3s 内结束。

安全网：即使用户忘了 `Bind`、只 `app.Register(reg)`，只要
`DrainTimeout > 0`（drainChecker 在健康检查聚合里），Registrar 内部的
`watchDrain` 会 50ms 轮询 `errors.Is(err, lynx.ErrDraining)` 并注销。
`DrainTimeout=0` 时 drainChecker 不进聚合，该安全网不生效，只能靠
`OnDrain`（Bind）或 `Stop`。

## 7.8 健康模型：三条通道

Registrar 的健康语义与探针是三条独立通道，不要混为一谈：

1. **心跳**：`heartbeat_interval`（默认 10s）刷新 TTL；连续失败 ≥3 次
   时 `CheckHealth` 返回 `ErrHeartbeatFailed`；未注册/已注销返回
   `ErrNotRegistered`。
2. **readiness**：Registrar 实现 `lynx.Checker`，上述错误参与 readiness
   聚合（HTTP `/healthz/readiness` 503）。`affect_readiness: false`
   时 `CheckHealth` 恒返回 nil（退出聚合）。
3. **liveness**：框架的 `/healthz/liveness` 本就不消费 Checker，
   注册中心抖动**永远**不会让进程被 kubelet 重启。

注册中心侧 check 的摘流延迟（选型的关键依据）：

| 检查方式 | 排水后何时变红 | 要求 |
| --- | --- | --- |
| Consul `http` → `/healthz/readiness` | **立即**（readiness 每次请求都实时读检查器） | **有 HTTP 端口时的推荐 check** |
| Consul `grpc` → `grpc.health.v1` | 最多一个 `HealthCheckPeriod`（**默认 10s**轮询） | **禁止**作为 gRPC-only 进程的唯一摘流手段 |
| Consul `ttl` + `OnDrain` Deregister | 主动 delete，Watch 立即推空 | **gRPC-only 进程必选**（TTL check + 排水注销） |

「排水开始 → 目录也 critical」只对 HTTP check 成立。gRPC-only 进程若
配 `health_check.type: grpc` 且 Deregister 缓慢/失败，发现客户端会在
几乎整个排水窗口内继续打到本实例——因此 gRPC-only 必须
`type: ttl` + `registry.Bind`（挂 OnDrain 注销）。

## 7.9 客户端发现（registry://）

Resolver 带进程内缓存：每个服务名一条缓存 + 一个后台 watch goroutine
（Watch 失败回退轮询）。Watch 断开期间继续提供最后一次成功快照
（stale-while-revalidate），快照年龄超过 `WithStaleMaxAge`（默认 60s
= 2 × 默认 `heartbeat_ttl`）即丢弃并返回 `ErrNoInstance`——分区后
不会无限供应死实例。

### HTTP

URI 语法 `registry://<service-name>/<path>`，保留查询键 `protocol`
（只允许 `http`/`https`，默认 http，无 http Endpoint 时回落 https）：

```go
rslv := registry.NewResolver(disc)
cli := clienthttp.New(clienthttp.WithClientOptions(func(c *gohttp.Client) {
    // c.Transport 此时已是 otelhttp.Transport；registry 改写包在其外层，
    // span 看到的是改写后的真实目标
    c.Transport = registry.NewHTTPTransport(rslv).Wrap(c.Transport)
}))
resp, err := cli.Get(ctx, "registry://order-service/v1/orders/1")
```

Transport 每次 `RoundTrip` 都 clone 请求并重新解析：不改调用方的
`*http.Request`，`WithRetry` 的每次重试也会重新选实例，不会钉死在
第一次选中的实例上。默认仍**不重试**（`client/http` 语义不变）。

### gRPC

target 语法 `registry:///<service-name>`（Host 必须为空，服务名在
path），经 `grpc.WithResolvers` 逐连接接入（不改 `client/grpc` 源码）：

```go
b := registry.NewGRPCBuilder(rslv)
conn, err := clientgrpc.Dial("registry:///user-service",
    clientgrpc.WithDialOptions(
        grpc.WithResolvers(b),
        grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
    ),
)
```

gRPC resolver 按 5s 周期把 Resolver 缓存翻译成连接地址；解析出错时
保留上一次地址状态（gRPC resolver 对暂态错误的惯例），服务下线（空
快照）则立即生效。

> **警告**：`resolver.Register(b)` 是进程全局副作用（注册 scheme 全局
> 表，测试与多 resolver 进程会撞车），只作为可选便利，不作为推荐入口；
> 请使用上面的 `grpc.WithResolvers` 逐连接方式。

`NewHTTPTransport` 与 `NewGRPCBuilder` 都必须吃 `*Resolver`（而非 raw
`Discovery`），两条路径共享同一套缓存 / stale 上限 / 默认 Filter。

## 7.10 DNS 后端与 Headless Service

`backend: dns` 时 `NewBackendFromConfig` 返回 `(nil, dnsDiscovery, nil)`
——DNS **只读**，没有 Registrar，不要 `Bind`。查询名为
`{name}.{namespace}.{domain}`；端口先查 SRV（`_http._tcp.…` 等，按
协议选服务标签），无 SRV 再查 A/AAAA，端口取自 `registry.dns.ports`
（缺省 http=8080、https=8443、grpc=9090）。Watch 即轮询
（`discovery.poll_interval`），NXDOMAIN 负缓存 TTL 钳制在 [5s, 30s]。

**ClusterIP vs Headless**：

- **ClusterIP Service**：通常只有一条 A 记录（Service VIP），
  kube-proxy 已经在做 LB。此时 Resolver + Picker 是冗余的——直接拨
  `http://user-service` 即可，不要开本模块。
- **Headless Service**（`clusterIP: None`）：每个 Pod 一条 A 记录，
  DNS 返回多实例，Picker 才有意义。这也是 K8s 内唯一值得开 DNS
  Discovery 的形态（外加多端口直连场景）。

DNS 后端无 version/tag/weight 概念；`IncludeUnhealthy` 对 DNS 无意义
（所有记录视为 Passing）。

## 7.11 失败模式摘要

| 场景 | 严重度 | 行为 | 缓解 |
| --- | --- | --- | --- |
| 启动时注册中心不可达 | 高 | 默认 `fail_fast: true`，`Start` 返回 error → 应用退出 | `fail_fast: false` 时 Start 继续阻塞、后台退避重试，成功前 readiness 红 |
| 运行中心跳失败 | 中 | 打点 + warn；连续 ≥3 次 → readiness 503（`affect_readiness` 可关） | TTL 到期目录摘除；不碰 liveness |
| 注销失败（关停） | 中 | 日志 + `ShutdownErrors`；依赖 TTL / `deregister_after` 兜底 | `DrainHookTimeout` 内一次 RPC + `Stop` 再试一次 |
| 脑裂 / 网络分区 | 高 | 分区侧心跳失败、TTL 后消失；客户端 Watch 断开后最多再用 stale 60s，其后 `ErrNoInstance` | Consul 默认 consistent；跨 DC 显式配置 |
| 同 ID 双副本互盖 | 高 | last-write-wins，无 fencing；客户端见地址抖动 | Downward API `metadata.name`；不要共用 hostname |
| 进程被 SIGKILL | 高 | 来不及注销 | TTL 30s + `DeregisterCriticalServiceAfter` 60s |
| 宣告 `:8080` 且无 host | 高 | `Init` 失败 | `advertise.host` 或 `LYNX_ADVERTISE_HOST` |
| 注册早于 Listen | 中 | 目录 check 失败 → critical | check 打 readiness；Advertiser 等待 `advertise_timeout` |
| Watch 中断 | 中 | 退避重连（1s–30s）；stale 最长 60s | 空快照立即失效；超时后不再用 stale |
| 发现打到已死实例 | 中 | 本次调用失败 | 默认 HTTP 不重试；`WithRetry` 时每次重试重新选实例；gRPC 等 resolver 更新 |
| **gRPC-only + Consul grpc check** | 高 | 排水后最多 10s 仍 SERVING | **改用 TTL check + `OnDrain` Deregister** |
| 注册中心与探针 LB 双路径 | 低 | HTTP 503 立即；目录在 Deregister/TTL | 目标行为（但不要与 kube-proxy LB 混用） |

## 7.12 Wire 集成

`boot.Bootstrap` 提供可选 setter `WithDrainHooks`（不改 `New` 签名）：

```go
boot.New(startHooks, stopHooks, services, factories).
    WithDrainHooks(boot.OnDrainHooks{reg.DeregisterHook()})
```

provider 示例（`_examples/boot` 风格）：

```go
func ProvideBackend(app lynx.App) (registry.Registry, registry.Discovery, error) {
    cfg := app.Config()
    if cfg.GetString("registry.backend") == "consul" {
        c, err := consul.NewFromConfig(cfg)
        if err != nil || c == nil {
            return nil, nil, err
        }
        return c, c, nil
    }
    return registry.NewBackendFromConfig(cfg)
}

func ProvideRegistrar(app lynx.App, wr registry.Registry, hs *http.Server) (*registry.Registrar, error) {
    return registry.NewFromConfig(app.Config(), wr, registry.HTTP(hs, registry.ProtocolHTTP))
}

func ProvideServices(hs *http.Server, r *registry.Registrar) []lynx.Service {
    if r == nil {
        return []lynx.Service{hs}
    }
    return []lynx.Service{r, hs}
}
```

`OnDrain` 钩子由 `registry.Bind` 或 `NewOnDrains` provider 挂载，
**不能**在服务 `Init` 里挂（`Init` 只读 `AppContext`，见
[第 3 章](./03-core-concepts.md) 3.6 节）。

## 7.13 CLI 约定

长期服务的 setup 调用 `registry.Bind`；`app.Command` / 一次性 CLI 的
setup **不要**调用 `Bind`（约定，无配置开关）：`affect_readiness=true`
时 command 会空等注册中心就绪，且 CLI 会向目录写下一条短命记录。

## 下一步

- [设计文档](./design-service-registry.md) - 完整设计论证、API 变更与 PR 切分
- [示例代码](../_examples/registry/) - memory 后端的最小可运行示例
- [第 3 章：核心概念](./03-core-concepts.md) - 生命周期、Drain 与关停时序
- [第 6 章：客户端](./06-clients.md) - HTTP/gRPC 客户端基础（超时、重试、传播）
