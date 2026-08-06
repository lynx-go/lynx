# PubSub 配置驱动装配（开箱即用）设计

日期：2026-08-06
状态：已批准

## 背景与目标

`_examples/pubsub/main.go` 目前有约 46 行手工装配样板：加载 `kafka` 配置段、创建 Kafka Transport、创建内存 Transport、创建 Broker（自动路由 + 默认回退）、加载 `pubsub.routes` 路由表、校验 transport 标识、逐条应用 `RouteKey`、注册 4 个组件。这些配置过程应内化到框架中，实现"开箱即用"：用户只需提供 transports 与 handlers，其余由配置驱动。

目标：示例装配代码从约 46 行降到约 10 行；Wire boot 应用中每个 provider 一行转发。

## 约束

1. **模块边界**：`contrib/pubsub` 与 `contrib/kafka` 是独立 go.mod，kafka 依赖 pubsub（单向）。pubsub 包无法感知 Kafka——kafka 的配置加载必须由 kafka 包自供，由调用方注入。
2. **API 冻结（v1.0）**：现有 `NewBroker`/`NewTransport`/`NewRouter`/`Broker` 接口/`Transport` 接口签名一律不动。新增必须是增量添加。给接口加方法即为破坏性变更，故**不改任何接口**。
3. **Wire 适配**：`_examples/boot` 的 Wire 模式要求 provider 是纯函数 `func(依赖...) (值, error)`。装配 API 必须天然是 Wire provider 的形状。

## 方案取舍记录

- **配置驱动构造函数（采用）** vs **装配器组件（否决）**：装配器把 broker/transports 藏进不透明组件，与 Wire 的类型图匹配、显式依赖注入哲学冲突（需 `set.Broker()` 式访问器绕行），且把注册时序藏进 Init 难以推理。构造函数是纯 `(值, error)` 函数，直接可作 Wire provider。
- **类型命名 `Bundle`（采用）**：它是构建**结果**（broker + transports 打包），不是构建者。`Builder` 语义反转且与框架已有 `lynx.ComponentBuilder`/`lynx.NewBuilder()` 撞名，否决。
- **内置 memory 默认（采用）**：`NewFromConfig` 未收到 `memory` 标识 transport 时，内置创建一个内存 Transport 作为默认回退并纳入注册列表。本地开发零配置可用。
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
// Bundle 是配置驱动装配的结果：Broker 与需要随应用注册的 Transports。
type Bundle struct {
	Broker     Broker
	Transports []Transport
}

// Components 返回应注册的全部组件（Transports + Broker），供 app.Register 使用。
func (b *Bundle) Components() []lynx.Component

// NewFromConfig 从配置装配消息组件：
//   - "pubsub" 段 routes（逻辑 topic → {transport, key}）逐条应用 RouteKey，
//     引用未提供的 transport 标识时报错（沿用现有错误语义）；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport 兼作默认回退；未提供时内置创建一个
//     内存 Transport 作为默认回退并纳入 Transports；
//   - map 中的字面 nil 值条目被防御性跳过；kafka 未启用的过滤由调用方
//     完成（示例 `if kafkaT != nil` 写法）。注意：具体类型 nil 指针赋给
//     Transport 接口（typed nil）无法在此检测，调用方必须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (*Bundle, error)
```

## 内部行为

`NewFromConfig` 装配流程（全部基于现有 broker 语义，无新时序）：

1. `UnmarshalKey("pubsub", &routesCfg)` 读路由表（缺段 = 无显式路由，不报错）；
2. 构建 broker options：`Transports` = 传入 map 的非 nil 值；`DefaultTransport` = `map["memory"]`（存在且非 nil 时），否则 `NewMemoryTransport()` 内置并追加进 `Transports` 结果；
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
// 每个 provider 一行转发，纯函数，Wire 类型图完整
func ProvideKafkaTransport(cfg lynx.Config) (*kafka.Transport, error) {
	t, err := kafka.NewFromConfig(cfg) // nil = 未启用，Wire 注入 nil 指针
	if t == nil {
		return nil, err
	}
	return t, err
}

func ProvideBundle(cfg lynx.Config, kafkaT *kafka.Transport) (*pubsub.Bundle, error) {
	transports := map[string]pubsub.Transport{}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	return pubsub.NewFromConfig(cfg, transports)
}

func NewComponents(b *pubsub.Bundle, router *pubsub.Router) []lynx.Component {
	return append(append([]lynx.Component{}, b.Transports...), b.Broker, router)
}
```

kafka 未启用时 Wire 注入 nil `*kafka.Transport`，`ProvideBundle` 过滤——依赖图完整，无魔法。

## 示例更新（_examples/pubsub/main.go 最终形态）

```go
kafkaT, err := kafka.NewFromConfig(app.Config()) // nil = 未配置，禁用
if err != nil {
	return err
}
transports := map[string]pubsub.Transport{}
if kafkaT != nil {
	transports["kafka"] = kafkaT
}
b, err := pubsub.NewFromConfig(app.Config(), transports)
if err != nil {
	return err
}
app.Register(b.Components()...)
app.Register(pubsub.NewRouter(b.Broker, []pubsub.Handler{&helloHandler{}, &notifyHandler{}}))
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
- key 别名生效（`notify → user_notify`）
- 未知 transport 标识报错
- map 含字面 nil 值条目时被防御性跳过（typed nil 不在框架职责内）
- kafka 未启用但路由仍引用 "kafka" 时按未知标识报错
- 无显式路由时默认回退内存
- 未提供 memory 时内置创建，且出现在 `Transports` 中
- 提供 memory 标识时复用之，不重复创建
- `Components()` 顺序稳定（transports 先、broker 后）

### 示例与文档

- 更新 `_examples/pubsub/main.go`、`_examples/pubsub/README.md` 关键代码点
- 更新 `docs/04-component-system.md` 的 pubsub 段（该文件工作区已有未提交修改）

## 不做的事（YAGNI）

- 不改 config.yaml schema（`kafka`/`pubsub` 段保持现状）
- 不把 handler 注册配置化（handlers 是代码，`NewRouter` 保持显式）
- 不给 `Transport`/`Broker` 接口加方法
- 不加"全自动"装配器组件（与 Wire 冲突，见方案取舍记录）
- 不新增 `pubsub` 段的 retry/marshaler 配置项（现有 Options 已支持，按需再扩）
