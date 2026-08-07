# PubSub 配置驱动装配（开箱即用）设计

日期：2026-08-06
状态：已批准（2026-08-06 修订：移除 Bundle，transports 一律显式——见"方案取舍记录"与修订记录）

## 背景与目标

`_examples/pubsub/main.go` 目前有约 46 行手工装配样板：加载 `kafka` 配置段、创建 Kafka Transport、创建内存 Transport、创建 Broker（自动路由 + 默认回退）、加载 `pubsub.routes` 路由表、校验 transport 标识、逐条应用 `RouteKey`、注册 4 个服务。这些配置过程应内化到框架中，实现"开箱即用"：用户只需提供 transports 与 handlers，其余由配置驱动。

目标：示例装配代码从约 46 行降到约 10 行；Wire boot 应用中每个 provider 一行转发。

## 约束

1. **模块边界**：`contrib/pubsub` 与 `contrib/kafka` 是独立 go.mod，kafka 依赖 pubsub（单向）。pubsub 包无法感知 Kafka——kafka 的配置加载必须由 kafka 包自供，由调用方注入。
2. **API 冻结（v1.0）**：现有 `NewBroker`/`NewTransport`/`NewRouter`/`Broker` 接口/`Transport` 接口签名一律不动。新增必须是增量添加。给接口加方法即为破坏性变更，故**不改任何接口**。
3. **Wire 适配**：`_examples/boot` 的 Wire 模式要求 provider 是纯函数 `func(依赖...) (值, error)`。装配 API 必须天然是 Wire provider 的形状。

## 方案取舍记录

- **配置驱动构造函数（采用）** vs **装配器服务（否决）**：装配器把 broker/transports 藏进不透明服务，与 Wire 的类型图匹配、显式依赖注入哲学冲突（需 `set.Broker()` 式访问器绕行），且把注册时序藏进 Init 难以推理。构造函数是纯 `(值, error)` 函数，直接可作 Wire provider。
- **不引入装配结果类型（最终采用）**：初版设计曾用 `Bundle`（broker + transports 打包，含 `Services()`），后经评审移除。移除理由：①`Bundle` 的必要性完全源于"隐式内置 memory transport"这一个设计选择——内置 transport 必须逃逸出构造函数（否则永不 Start/Stop，健康检查挂红、gochannel 泄漏），且 Wire provider 单值约束（wire guide：`(T, error)`）要求打包；②改为 **transports 一律显式**（kafka 与 memory 对称，都是调用方创建、注册），`NewFromConfig` 退化为纯"配置 → Broker"构造，返回 `Broker` 单值，天然 Wire 友好；③概念减负——pubsub 少 1 个类型 + 1 个方法，"memory 特殊"从类型级降为一行文档约定。代价：直接用户多 2 行（显式创建并注册 memory transport）。
- **内置 memory 默认（初版采用 → 修订为显式）**：初版"未提供 memory 时内置创建并纳入注册列表"已修订——不再内置创建任何 transport；`transports["memory"]`（提供且非 nil 时）兼作默认回退的文档约定**保留**，保住"未路由 topic 兜底"与本地开发体验。
- **kafka 无配置段即禁用（采用）**：`kafka.NewFromConfig` 读不到 `kafka` 段（或段为空、无任何 topic）时返回 `(nil, nil)`。调用方判定后不注册 kafka，config.yaml 删掉 kafka 段即可纯内存运行。
- **transport 标识绑定用 `map[string]Transport`（采用）**：路由表 `{transport, key}` 的标识需要名字绑定，map key 即标识；无需给 `Transport` 接口加 `Identifier()` 方法。

## API 规格

### contrib/kafka —— 新增 `fromconfig.go`

```go
// NewFromConfig 从配置 "kafka" 段加载 Options 并创建 Transport。
// 段缺失或为空（无任何 topic）时返回 (nil, nil)，表示 Kafka 未启用。
func NewFromConfig(cfg lynx.Config) (*Transport, error)
```

实现：`UnmarshalKey("kafka", &opts)`；`len(opts.Topics) == 0` → `(nil, nil)`；否则 `NewTransport(opts)`。

返回具体类型 `*Transport`——"未启用"的 nil 判定在调用方是安全的（`kafkaT == nil`），不存在 typed-nil 陷阱。

### contrib/pubsub —— 新增 `builder.go`

```go
// NewFromConfig 从配置装配 Broker：
//   - "pubsub" 段 routes（逻辑 topic → {transport, key}）逐条应用 RouteKey，
//     引用未提供的 transport 标识时报错（沿用现有错误语义）；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport（提供且非 nil 时）兼作默认回退——未路由
//     的 topic 走它；不提供则无默认回退，未路由 topic 发布报错；
//   - 不创建任何 transport：kafka 与 memory 一律由调用方创建并注册
//     （生命周期归属应用）；
//   - map 中的字面 nil 值条目被防御性跳过；kafka 未启用的过滤由调用方
//     完成（示例 `if kafkaT != nil` 写法）。注意：具体类型 nil 指针赋给
//     Transport 接口（typed nil）无法在此检测，调用方必须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (Broker, error)
```

## 内部行为

`NewFromConfig` 装配流程（全部基于现有 broker 语义，无新时序）：

1. `UnmarshalKey("pubsub", &routesCfg)` 读路由表（缺段 = 无显式路由，不报错）；
2. 构建 broker options：`Transports` = 传入 map 的非 nil 值（`memory` 条目也计入）；`DefaultTransport` = `map["memory"]`（存在且非 nil 时），否则 nil；
3. `NewBroker(options)`——`Init` 期自动路由照常运行（传入 transport 声明的 topic 自动路由，显式 RouteKey 覆盖）；
4. 逐条 route 应用：标识在 map 中不存在（包括 kafka 未启用却仍引用 "kafka"）→ 报错 `pubsub: route %q references unknown transport %q`（沿用现有错误消息与构建期报错语义）；`key` 为空 → 缺省为 topic 名。

## 错误路径

| 场景 | 行为 |
| --- | --- |
| kafka 段格式非法（如 `initial_offset: bogus`） | `NewFromConfig` 构建时报错（较现有 `Init` 期校验略有前移，更早失败） |
| route 引用未知 transport 标识 | 构建期报错，避免路由表静默失真 |
| kafka 未启用但路由仍引用 "kafka" | 同未知标识，报错（禁用 = 未提供） |
| `pubsub` 段缺失 | 无显式路由，仅自动路由 + 默认回退，不报错 |

## Wire boot 适配（provides.go 形态）

```go
// 每个 provider 一行转发，纯函数，Wire 类型图完整；命名统一 New* 前缀
func NewKafkaTransport(cfg lynx.Config) (*kafka.Transport, error) {
	return kafka.NewFromConfig(cfg) // nil = 未启用，Wire 注入 nil 指针
}

func NewMemoryTransport() *pubsub.MemoryTransport {
	return pubsub.NewMemoryTransport()
}

func NewBroker(cfg lynx.Config, kafkaT *kafka.Transport, memT *pubsub.MemoryTransport) (pubsub.Broker, error) {
	transports := map[string]pubsub.Transport{"memory": memT}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	return pubsub.NewFromConfig(cfg, transports)
}

func NewServices(memT *pubsub.MemoryTransport, kafkaT *kafka.Transport,
	broker pubsub.Broker, router *pubsub.Router, hs *http.Server) []lynx.Service {
	comps := []lynx.Service{memT}
	if kafkaT != nil {
		comps = append(comps, kafkaT)
	}
	return append(comps, broker, router, hs)
}
```

kafka 未启用时 Wire 注入 nil `*kafka.Transport`，`NewBroker`/`NewServices` 过滤——依赖图完整，每个节点类型显式可见，无魔法。

## 示例更新（非 Wire 直接使用形态）

```go
kafkaT, err := kafka.NewFromConfig(app.Config()) // nil = 未配置，禁用
if err != nil {
	return err
}
memT := pubsub.NewMemoryTransport() // 显式创建，与 kafka 对称
transports := map[string]pubsub.Transport{"memory": memT}
if kafkaT != nil {
	transports["kafka"] = kafkaT
}
broker, err := pubsub.NewFromConfig(app.Config(), transports)
if err != nil {
	return err
}
app.Register(memT)
if kafkaT != nil {
	app.Register(kafkaT)
}
app.Register(broker)
app.Register(pubsub.NewRouter(broker, []pubsub.Handler{&helloHandler{}, &notifyHandler{}}))
```

`config.yaml` 的 `kafka`/`pubsub` 段 schema 不变，现有配置兼容。

## 测试计划

### contrib/kafka（新增 `fromconfig_test.go`，沿用现有 viper 配置加载模式）

- 段缺失 → `(nil, nil)`
- 段为空（`kafka: {}`）→ `(nil, nil)`
- 正常段 → 返回 Transport，`Topics()` 与配置一致
- 复用现有 `TestOptionsFromConfig` 的加载方式

### contrib/pubsub（新增 `builder_test.go`）

- 显式路由生效（`hello → kafka`，发布/解析命中 kafka Transport）
- key 别名生效（`notify → user_notify`）；key 缺省为 topic 名
- 未知 transport 标识报错
- map 含字面 nil 值条目时被防御性跳过（typed nil 不在框架职责内）
- kafka 未启用但路由仍引用 "kafka" 时按未知标识报错
- 提供 memory 标识时未路由 topic 回退到它（默认回退约定）
- 未提供 memory 时未路由 topic 发布报 "no transport routed"
- 不创建任何 transport：返回类型为 `Broker`（无 Transports 列表断言）

### 示例与文档

- 更新 `_examples/pubsub/main.go`、`_examples/pubsub/README.md` 关键代码点
- 更新 `docs/04-component-system.md` 的 pubsub 段（该文件工作区已有未提交修改）

## 不做的事（YAGNI）

- 不改 config.yaml schema（`kafka`/`pubsub` 段保持现状）
- 不把 handler 注册配置化（handlers 是代码，`NewRouter` 保持显式）
- 不给 `Transport`/`Broker` 接口加方法
- 不加"全自动"装配器服务（与 Wire 冲突，见方案取舍记录）
- 不新增 `pubsub` 段的 retry/marshaler 配置项（现有 Options 已支持，按需再扩）
- 不内置创建任何 transport（kafka 与 memory 一律显式，生命周期归属应用；"memory" 标识兼作默认回退仅是一行文档约定）
