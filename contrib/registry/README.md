# registry

模块路径：`github.com/lynx-go/lynx/contrib/registry`

服务注册与发现的可选 contrib：注册发现的数据模型与接口（`Registry` / `Discovery`）、进程级注册服务 Registrar、客户端发现 Resolver，以及 memory / DNS 两个零依赖后端。生产级 Consul 后端在 [contrib/consul](../consul/README.md)。

## 能力要点

- 数据模型：一进程一条 `Instance`（可挂多条 `Endpoint`，`registry.go:99`）；`Filter` 零值即安全默认（只保留 `StatusPassing`），`MatchFilter` 是全部后端与 Resolver 共用的过滤实现（`registry.go:121`）；接口 `Registry`（写，`registry.go:146`）、`Discovery`（读，`registry.go:155`）、`Watcher`（推送）、`Advertiser`（宣告地址，`registry.go:169`）
- `*Registrar`：实现 `lynx.Service` + `lynx.Checker`（`registrar.go:155`）。`Start` 注册并维持心跳（默认 10s）后阻塞；`Stop` 幂等注销；`DeregisterHook()` 供 `OnDrain` 排水即摘除（`registrar.go:495`）。心跳连续失败 ≥3 次只影响 readiness，不影响 liveness
- `*Resolver`：进程内缓存 + 每服务名一个后台 watch goroutine（断开按 1s–30s 退避重连、Watch 不可用回退轮询）；分区期间继续供应最后一次快照，超过 stale 上限（默认 60s）丢弃并返回 `ErrNoInstance`（`resolver.go:74`）
- Picker：`RoundRobinPicker()`（Resolver 默认）与 `RandomPicker()`（`picker.go`），v1 均忽略 `Instance.Weight`；后端：`NewMemory()` 进程内 Registry + Discovery（`memory.go:34`）、`NewDNSDiscovery(...)` 只读 DNS（SRV 优先，A/AAAA 回落，`dns.go:115`，适配 K8s Headless Service）
- 客户端接入：`NewHTTPTransport(rslv).Wrap(base)` 把 `registry://<service>/<path>` 请求改写为实例地址（`http_transport.go:29`）；`NewGRPCBuilder(rslv)` 提供 scheme 为 `registry` 的 gRPC resolver Builder（`grpc_resolver.go:59`）。两者都必须吃 `*Resolver`，共享同一套缓存
- Advertiser：`HTTP(hs, protocol)` / `GRPC(gs)` / `Static(protocol, hostPort)`（`advertiser.go:12`），`AdvertiseAddr()` 非空优先、否则回落 `Addr()`

## 快速开始

memory 后端（完整可运行版见 [_examples/registry](../../_examples/registry/)）：

```yaml
# config.yaml
addr: "127.0.0.1:8080"
registry:
  backend: memory
```

```go
runner := lynx.NewRunner(func(app lynx.App) error {
	hs := http.NewServer(mux,
		http.WithAddr(app.Config().GetString("addr")),
		http.WithHealthCheckers(app.HealthCheckers),
	)

	// 后端：memory 同时作为 Registry 与 Discovery
	wr, disc, err := registry.NewBackendFromConfig(app.Config())
	if err != nil {
		return err
	}
	// Registrar：HTTP 服务器包装为 Advertiser；Apply = Register + OnDrain
	// 注销钩子（nil 时 no-op）
	reg, err := registry.NewFromConfig(app.Config(), wr,
		registry.HTTP(hs, registry.ProtocolHTTP),
	)
	if err != nil {
		return err
	}
	registry.Apply(app, reg)
	app.Register(hs)

	// 客户端：Resolver + registry:// Transport
	rslv := registry.NewResolver(disc)
	cli := clienthttp.New(clienthttp.WithClientOptions(func(c *gohttp.Client) {
		c.Transport = registry.NewHTTPTransport(rslv).Wrap(c.Transport)
	}))
	_, _ = cli.Get(ctx, "registry://my-app/hello") // 按服务名寻址
}, lynx.WithDrainTimeout(3*time.Second))
```

gRPC 侧 target 为 `registry:///<service-name>`，经 `grpc.WithResolvers` 逐连接接入：

```go
conn, err := clientgrpc.Dial("registry:///user-service",
	clientgrpc.WithDialOptions(
		grpc.WithResolvers(registry.NewGRPCBuilder(rslv)),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	),
)
```

## 配置

`NewBackendFromConfig` / `NewFromConfig` 读取 `registry` 段（`fromconfig.go:12`）；段缺失、`enabled: false` 或 `backend: ""` 时返回 nil——未启用即零开销。

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `registry.enabled` / `registry.backend` | 段存在即启用 / `""` | `backend` 取 `memory` / `dns`；`consul` 在此报错，用 `consul.NewFromConfig` |
| `registry.fail_fast` / `registry.affect_readiness` | `true` / `true` | 首次注册失败时 Start 是否返回错误 / 是否参与 readiness 聚合 |
| `registry.heartbeat_interval` | `10s` | 心跳间隔；与 `heartbeat_ttl` 交叉校验 interval < ttl |
| `registry.heartbeat_ttl` / `registry.deregister_after` | 30s / 60s（consul 侧默认） | 由 contrib/consul 读取，Registrar 只做交叉校验 |
| `registry.advertise_timeout` | `5s` | 等待 Advertiser 出现非空 Endpoints 的上限 |
| `registry.tags` / `registry.meta` / `registry.weight` | — / — / `100` | 写入目录的标签 / 元数据 / 权重（v1 内置 Picker 忽略） |
| `registry.advertise.host` | — | 补全裸端口 endpoint；空则直读 `LYNX_ADVERTISE_HOST`（不经 Viper） |
| `registry.endpoints` | — | 静态 endpoint 列表（`protocol` + `address`，生产推荐） |
| `registry.service_name` / `registry.instance_id` | — | 覆盖 `service.name` / `service.id` |
| `registry.discovery.poll_interval` | `15s` | DNS 后端 Watch 的轮询间隔 |
| `registry.dns.domain` / `registry.dns.namespace` | `svc.cluster.local` / `default` | 查询名 `{name}.{namespace}.{domain}` |
| `registry.dns.ports` | http=8080, https=8443, grpc=9090 | 无 SRV 时按协议补端口（在默认表上逐项覆盖） |

## 与 lynx 核心的集成

- `Registrar` 本身是 `lynx.Service`：`registry.Apply(app, reg)`（`fromconfig.go:164`）= `app.Register(reg)` + `app.OnDrain(reg.DeregisterHook())`，排水置位那一刻即从目录删除实例；使用 `Apply` 必须显式 `WithDrainTimeout`（>0），否则 `Run()` 启动期报 `lynx.ErrDrainHooksRequireDrainTimeout`
- 安全网：只 `app.Register(reg)` 忘了 `Apply` 时，`DrainTimeout > 0` 下 Registrar 内部 `watchDrain`（`registrar.go:430`）轮询 `lynx.ErrDraining` 并注销
- 注册发生在 `Start`（等真实监听地址），不要在 `OnPreStart` 手动注册；CLI（`app.Command`）的 setup 约定不要调用 `Apply`
- 宣告地址禁止裸 `:8080`：优先静态 endpoints → `advertise.host` → `LYNX_ADVERTISE_HOST` → Advertiser 的 `AdvertiseAddr()`/`Addr()`，全缺失时 `Init` 失败

## 相关文档与示例

- [docs/07-registry.md](../../docs/07-registry.md) — 注册发现教程（健康模型、排水时序、失败模式）；[docs/design-service-registry.md](../../docs/design-service-registry.md) — 设计论证
- [_examples/registry](../../_examples/registry/) — memory 后端最小可运行示例
- [contrib/consul](../consul/README.md) — Consul 生产后端
