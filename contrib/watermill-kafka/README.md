# watermill-kafka

模块：`github.com/lynx-go/lynx/contrib/watermill-kafka`（独立 Go module；包名 `kafka`，建议 `import wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"`，见 `transport.go:4`）

Kafka 的 `eventbus.Transport`：按逻辑 topic 配置集群、物理主题与消费/发布参数，配合 [contrib/watermill](../watermill/README.md) 的 Bus 实现跨进程事件投递。

## 能力要点

- `NewTransport(opts Options) (*Transport, error)`（`transport.go:186`）：`Options.Topics` 为 `map[逻辑topic]TopicOptions`（`transport.go:40`）
- `NewFromConfig(cfg lynx.Config) (*Transport, error)`（`fromconfig.go:13`）：从 `kafka:` 段装配；**段缺失或为空返回 `(nil, nil)` 表示未启用，返回 nil 时不得 Register**
- `NewBusFromConfig(cfg lynx.Config) (eventbus.Bus, []lynx.Service, error)`（`fromconfig.go:34`）：kafka 版总线装配入口，签名直接匹配 `lynx.WithBusProvider`；始终含 `"memory"` transport（兼作 DefaultTransport），`kafka:` 段启用时把 Transport 作为配套服务返回（见[快速开始](#快速开始)）
- 同时实现 `eventbus.Transport`、`lynx.Service`、`lynx.Checker`、`lynx.Ready`（`transport.go:925`）：Register 后 Start/Stop、健康聚合、就绪等待由框架托管
- 订阅按（消费组 × 物理 topic × 实例数）展开并 fan-in 到单一 channel（`transport.go:393`）；每实例一条独立消费组连接，上限 64（`transport.go:516`）
- Kafka record Key = `Event.Key` / `eventbus.WithMessageKey`（`marshaler.go:14`），同键同分区有序；消费侧缺 header 时从 record Key 回填
- SASL：`PLAIN`（缺省）/ `SCRAM-SHA-256` / `SCRAM-SHA-512`（`transport.go:533`、`scram.go`）；TLS：自定义 CA / ServerName / 跳过校验（`transport.go:546`）
- `Init` 为每个 topic 预构建两侧 sarama 配置并 `Validate()`（`transport.go:231`）：非法配置启动期即报错，不等首次收发才暴露
- 客户端按 brokers 分组共享、先构建者生效；同集群多 topic 的差异配置经指纹比对记 Warn（`transport.go:147`、`transport.go:659`）

## 快速开始

推荐经 `lynx.WithBusProvider` 注入（完整可运行示例见 [_examples/bus-kafka](../../_examples/bus-kafka)）。kafka 版装配入口 `NewBusFromConfig` 的返回值直接匹配 provider 签名，应用侧一行接入：

```go
package main

import (
	"github.com/lynx-go/lynx"
	wmkafka "github.com/lynx-go/lynx/contrib/watermill-kafka"
)

func main() {
	lynx.NewRunner(func(app lynx.App) error {
		// app.Register(...)
		return nil
	},
		lynx.WithName("my-app"),
		lynx.WithBusProvider(wmkafka.NewBusFromConfig),
	).Run()
}
```

```yaml
bus:
  topics:
    order.created:
      route: { transport: kafka, key: order.created }  # key 引用 kafka: 段同名条目
kafka:
  order.created:
    brokers: ["127.0.0.1:9092"]
    topics: [orders_v1]
    consumer: { group_id: my-app, instances: 1 }
    producer: { log_message: true }
```

### 手工装配（自定义 transport）

`NewBusFromConfig` 固定组合 `"memory"` + `"kafka"` 两个 transport；需要额外后端或替换 memory 时手工组装（`kafka:` 段缺失时 `NewFromConfig` 返回 `(nil, nil)`，此时不加入 transports、不注册）：

```go
// import: lynx "github.com/lynx-go/lynx"、watermill、wmkafka、
// "github.com/lynx-go/lynx/eventbus"
func busFromConfig(cfg lynx.Config) (eventbus.Bus, []lynx.Service, error) {
	kafkaT, err := wmkafka.NewFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	transports := map[string]eventbus.Transport{"memory": watermill.NewMemoryTransport()}
	var svcs []lynx.Service
	if kafkaT != nil {
		transports["kafka"] = kafkaT
		svcs = append(svcs, kafkaT) // Transport 生命周期由框架托管
	}
	bus, err := watermill.NewFromConfig(cfg, transports)
	return bus, svcs, err
}
```

业务代码与内存 Bus 无差别：`eventbus.NewTopic[T]` + `Subscribe` / `Publish`；逐消息 Kafka 分区键用 `eventbus.WithMessageKey`（同键分区有序）。`lynx.*` 生命周期事件强制内存 Transport，route 到 kafka 会在 Bus `Init` 期报错。

## 配置（`kafka:` 段）

`kafka:` 段整体是 `map[逻辑topic]TopicOptions`（`mapstructure:",remain"`，`transport.go:41`）。同集群（brokers 相同）多 topic 共享一份客户端配置，SASL/TLS/收发参数须一致，差异配置会被忽略并 Warn。字段类型校验先于弱类型转换执行（`fromconfig.go:35`）：`brokers` / `topics` 写成整数标量、`consumer` / `producer` / `sasl` / `tls` 写成非映射会直接报错，不被 mapstructure 静默弱转。

顶层（每个逻辑 topic）：

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `kafka.<topic>.brokers` | []string | 必填 | 集群地址（`Init` 校验非空，`transport.go:239`） |
| `kafka.<topic>.topics` | []string | 必填 | 订阅的物理 topic 列表，多 topic fan-in |
| `kafka.<topic>.consumer` | mapping | — | 消费侧；缺省 = 该 topic 只发布 |
| `kafka.<topic>.producer` | mapping | — | 发布侧；缺省 = 该 topic 只订阅 |
| `kafka.<topic>.sasl` / `kafka.<topic>.tls` | mapping | — | 集群级认证（SASL / TLS） |

`consumer`（`transport.go:77`）：

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `group_id` | string | 空 | 消费组（kafka 后端配置）。订阅时组必须可得，否则报错；同一事件的多个 handler 共享该组与一条 transport 订阅 |
| `instances` | int | `1` | 消费组实例数（每实例一条独立连接）；上限 64，超出钳制并 Warn（`transport.go:422`） |
| `auto_commit_enabled` | bool | `true`（sarama 默认） | `false` = 每条消息 Ack 时显式提交 offset，`commit_interval` 不生效 |
| `commit_interval` | duration | sarama 默认 | offset 自动提交间隔 |
| `initial_offset` | string | `newest` | 首次消费起点：`oldest` / `newest` |
| `nack_resend_sleep` | duration | `100ms`（底层库默认） | Nack 后重投等待 |
| `reconnect_retry_sleep` | duration | `1s`（底层库默认） | 重连重试间隔 |
| `session_timeout` / `heartbeat_interval` | duration | sarama 默认 | 消费组会话超时 / 心跳间隔 |
| `fetch_min_bytes` / `fetch_max_bytes` | int | sarama 默认 | 单次 fetch 的最小 / 最大字节数 |
| `fetch_max_wait` | duration | sarama 默认 | broker 凑批最长等待 |
| `log_message` | bool | `false` | 收到消息打 Debug 日志 |
| `client_id` | string | 空 | sarama ClientID |

`producer`（`transport.go:109`）：

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `topic` | string | `topics[0]` | 发布物理 topic |
| `required_acks` | int | 未设置 | `1` = WaitForLocal，`-1` = WaitForAll；`0` 视为未设置（sarama 默认） |
| `batch_size` | int | 未设置 | 攒批条数 → sarama `Producer.Flush.Messages` |
| `flush_bytes` | int | 未设置 | 攒批字节数 → sarama `Producer.Flush.Bytes` |
| `flush_frequency` | duration | 未设置 | 攒批最长滞留 → sarama `Producer.Flush.Frequency` |
| `retry_max` | int | 未设置 | 发送失败重试次数 |
| `timeout` | duration | 未设置 | broker 等待应答时长 |
| `compression` | string | 未设置 | `none` / `gzip` / `snappy` / `lz4` / `zstd` |
| `log_message` | bool | `false` | 发送消息打 Debug 日志 |
| `client_id` | string | 空 | sarama ClientID |

`sasl`（`transport.go:58`）：

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `enabled` | bool | `false` | 开启 SASL |
| `mechanism` | string | `PLAIN` | `PLAIN` / `SCRAM-SHA-256` / `SCRAM-SHA-512`，其他值报错 |
| `user` / `password` | string | 空 | 账号 / 密码 |

`tls`（`transport.go:67`）：

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `enabled` | bool | `false` | 开启 TLS |
| `insecure_skip_verify` | bool | `false` | 跳过证书校验 |
| `ca_file` | string | 空 | 自签 CA 证书路径；空 = 系统信任库 |
| `server_name` | string | 空 | 覆盖 TLS 校验主机名；空 = broker 地址 |

## 关键语义

- **nil = 未启用**：`kafka:` 段缺失或为空时 `NewFromConfig` 返回 `(nil, nil)`；nil Transport 不得 Register，也不得放进 `watermill.NewFromConfig` 的 transports。
- **消费组语义**：同 `group_id` 多实例竞争消费（组内每条消息只投递给一个实例）；不同 `group_id` 各自收全量（跨服务 / 跨实例扇出）。同一进程内同一逻辑 topic 只建一条 transport 订阅（Bus 侧订阅复用），组冲突结构上不可能。已知边界：物理 topics 重叠的不同逻辑 topic 共享同一 kafka 条目的组时会互相瓜分——为它们拆成不同 kafka 条目。
- **确认与提交顺序**：Ack 触发 `MarkMessage(offset+1)`（`auto_commit_enabled=false` 时立即 `Commit`），提交高 offset 隐含提交更低 offset。Bus 侧 `max_in_flight>1` 会乱序确认，崩溃可能跳过仍在处理的低 offset 消息——严格 at-least-once 请保持默认 `max_in_flight=1`（串行）。
- **毒消息止损**：handler 终态失败后 Nack 重投（默认约 100ms 一轮）；上限由 `bus.max_redeliveries`（默认 10）控制，超过后 Bus 记 Error 并 Ack 丢弃。详见 [watermill README](../watermill/README.md)。
- **Ready 就绪信号**（`transport.go:290`）：`Ready()` 返回一次性 channel，`Start` 跨过启动门槛（置位运行标志）后关闭；未启动或 `Init` 失败不关闭。命令依赖等待经此事件驱动等待，无需轮询。
- **健康检查**：`CheckHealth` 仅是进程内运行标志（`transport.go:323`），不做 broker 连通性检查——断连要等 Publish/Subscribe 报错才暴露。
- **Stop 后拒绝收发**：`Stop` 后 Publish / Subscribe 返回 `"kafka transport is stopped"`（`transport.go:354`），而非底层已关闭客户端的 sarama 报错。
- **`lynx.*` 禁止 route 到 Kafka**：watermill Bus 的 Init 校验直接报错；生命周期事件只走进程内内存 Transport。

## 与 lynx 核心的集成

- `NewBusFromConfig` 是 `WithBusProvider` 的 kafka 版装配入口：返回的 `[]lynx.Service` 含 Transport，按 Register 语义托管——Init 同步执行、Start/Stop 纳入生命周期、实现 `Checker` 的进入健康聚合（`options.go:352`），CLI 命令的依赖等待因此能等 Transport 就绪。
- 代码直接构造：`wmkafka.NewTransport(opts)` 后 `app.Register(kafkaT)`。注意 `Bus.Stop` 不关闭传入的 Transports（`watermill/bus.go:239`），漏注册则 Transport 永不关闭。
- 客户端惰性建立：首次 Publish/Subscribe 才创建连接；`Init` 仅离线校验配置，不触网（`transport.go:227`）。

## 相关文档

- [_examples/bus-kafka](../../_examples/bus-kafka)：完整可运行示例（含消费组语义的观察方法）
- [watermill](../watermill/README.md)：Bus 实现（`bus:` 段配置、重投上限、消费组互斥）
- [EventBus 设计](../../docs/design-eventbus.md)
- [主 README](../../README.md)
