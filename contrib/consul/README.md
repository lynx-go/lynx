# consul

模块路径：`github.com/lynx-go/lynx/contrib/consul`

服务注册发现的 Consul 生产后端：`*Client` 同时实现 `registry.Registry` 与 `registry.Discovery`，并经 `Client.Coordinator()` 提供 `cluster.Coordinator`（KV + Session）实现。

## 能力要点

- `New(config *api.Config, opts ...Option)` 构造 Client（`consul.go:173`）；`config.Token` 为空时直读官方环境变量 `CONSUL_HTTP_TOKEN`（优先于配置文件），token 绝不进日志
- 注册写路径：主端口取第一条匹配 check 协议的 Endpoint，其余 Endpoint 编码进 Meta 键 `lynx_endpoints`，读取时还原（仅本模块解析该键）；同 ID 重复注册是 last-write-wins upsert（`consul.go:207`）
- 健康检查类型 `CheckTypeTTL` / `CheckTypeHTTP` / `CheckTypeGRPC`（`consul.go:42`）。`Heartbeat` 仅 ttl 检查时发 `UpdateTTL`，http/grpc 被动探针时为 no-op（`consul.go:327`）
- 读路径 `GetService` / `Watch`（`consul.go:357` / `consul.go:370`）：blocking query（服务端等待上限 5min），默认 consistent 读（`allow_stale` 可开陈旧读）；错误按 1s–30s 退避重连
- 配置装配：`NewFromConfig(cfg)` 从 `registry` 段构造（`fromconfig.go:53`）；registry 段缺失 / 未启用 / `backend` 非 consul 时返回 `(nil, nil)`，与 `registry.NewBackendFromConfig`（后者遇 `backend: consul` 明确报错指向本函数，避免 contrib 依赖环）配合实现同一套代码切换后端
- 进程间协调：`Client.Coordinator(opts ...cluster.Option)` 返回 Consul KV + Session 实现的 `cluster.Coordinator`（`coordinator.go:26`），与 Registry 共用同一连接与 token；Session TTL 下限 `MinSessionTTL = 10s`（`coordinator.go:14`），短间隔任务用 [contrib/cluster-redis](../cluster-redis/README.md)

## 快速开始

由应用 setup 构造（`registry.NewBackendFromConfig` 不构造 consul 后端）：

```go
runner := lynx.NewRunner(func(app lynx.App) error {
	var wr registry.Registry
	var disc registry.Discovery
	switch app.Config().GetString("registry.backend") {
	case "consul":
		c, err := consul.NewFromConfig(app.Config()) // 未启用时返回 (nil, nil)
		if err != nil {
			return err
		}
		if c != nil {
			wr, disc = c, c // Client 同时实现 Registry 与 Discovery
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
		registry.Apply(app, reg) // wr==nil 时 NewFromConfig 返回 nil；Apply no-op
	}
	app.Register(hs, gs)
	return nil
}, lynx.WithDrainTimeout(15*time.Second))
```

代码直接构造（不经配置）：

```go
c, err := consul.New(api.DefaultConfig(), consul.WithCheckType(consul.CheckTypeTTL))
```

Coordinator（选主 / 单飞，配方见 [contrib/cluster](../cluster/README.md)）：

```go
coord := c.Coordinator(cluster.WithNamespace("my-app"), cluster.WithInstance("node-1"))
lease, ok, err := coord.Acquire(ctx, "leader", consul.MinSessionTTL)
```

## 配置

`NewFromConfig` 读取 `registry` 段中 consul 关心的子树（`fromconfig.go:14`）：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `registry.backend` | — | 非 `consul` 时返回 `(nil, nil)` |
| `registry.heartbeat_ttl` | `30s` | ttl 检查的 TTL；与 `heartbeat_interval` 交叉校验 interval < ttl |
| `registry.deregister_after` | `60s` | DeregisterCriticalServiceAfter |
| `registry.health_check.type` | `http` | `ttl` / `http` / `grpc` |
| `registry.health_check.path` | `/healthz/readiness` | http 检查路径 |
| `registry.health_check.interval` | `10s` | http/grpc 检查间隔 |
| `registry.health_check.timeout` | `3s` | http/grpc 检查超时 |
| `registry.consul.address` | Consul 默认 | 裸 `host:port` 或带 scheme 的 URL；显式 `http://` 与 `tls.enabled=true` 并存时报错 |
| `registry.consul.token` | — | 空时直读 `CONSUL_HTTP_TOKEN` |
| `registry.consul.datacenter` / `registry.consul.namespace` | — | 透传 Consul API 配置 |
| `registry.consul.allow_stale` | `false` | Watch/Get 默认 consistent 读 |
| `registry.consul.tls.{enabled,ca_file,cert_file,key_file,insecure_skip_verify}` | 全关 | TLS 一等配置，生产应开启 |

## 与 lynx 核心的集成

- Client 经 `registry.NewFromConfig` 交给 `Registrar` 托管：注册 / 心跳 / 排水注销（`OnDrain` 钩子）全部由 `registry.Apply` 挂载，应用不直接调 `Register`
- check 选型：有 HTTP 端口用 `type: http`（打 `/healthz/readiness`，排水摘流立即生效）；**gRPC-only 进程必须 `type: ttl` + `registry.Apply`**——grpc check 的摘流最多滞后一个 HealthCheckPeriod（默认 10s 轮询）
- SIGKILL 兜底：ttl 30s 过期 + `deregister_after` 60s 摘除
- `Coordinator` 不参与生命周期，直接在业务代码 / `cluster.Singleton` 中使用

## 相关文档与示例

- [docs/07-registry.md](../../docs/07-registry.md) — 7.5 节 Consul 接入、7.8 节健康模型三条通道
- [docs/design-service-registry.md](../../docs/design-service-registry.md) — 设计论证
- [_examples/registry](../../_examples/registry/) — memory 后端示例（后端切换模式相同）
- [contrib/registry](../registry/README.md) / [contrib/cluster](../cluster/README.md)
