# PubSub 配置驱动装配（开箱即用）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `_examples/pubsub/main.go` 的约 46 行装配样板（kafka 配置加载、broker 创建、路由表应用、组件注册）内化到框架：新增 `kafka.NewFromConfig` 与 `pubsub.NewFromConfig`/`Bundle`，配置驱动、开箱即用、Wire 适配。

**Architecture:** kafka 包新增配置加载构造函数（段缺失/为空返回 `(nil, nil)` 表示未启用）；pubsub 包新增 `Bundle`（Broker + 待注册 Transports）与 `NewFromConfig`（读 `pubsub.routes` 应用显式路由、非 nil transports 自动路由、内置内存 Transport 兜底）。现有 API 全部保留。

**Tech Stack:** Go 1.25+、viper（测试构造配置）、watermill、多模块 go.work（`contrib/pubsub` 与 `contrib/kafka` 独立 go.mod）

## Global Constraints

- 不改现有公共 API：`NewBroker`/`NewTransport`/`NewRouter`/`Broker` 接口/`Transport` 接口签名一律不动（v1.0 API 冻结）。
- `config.yaml` 的 `kafka`/`pubsub` 段 schema 不变，现有配置兼容。
- 错误消息沿用现有格式：`pubsub: route %q references unknown transport %q`。
- 注释、文档、提交消息用中文；提交消息遵循 conventional commits 并以 `Co-Authored-By: Claude <noreply@anthropic.com>` 结尾。
- 测试按模块运行：`cd contrib/<module> && go test -race ./...`；示例模块 `cd _examples && go build ./...`。
- 每个任务独立可测、独立提交。

---

### Task 1: kafka.NewFromConfig 配置加载构造函数

**Files:**
- Create: `contrib/kafka/fromconfig.go`
- Test: `contrib/kafka/fromconfig_test.go`

**Interfaces:**
- Consumes: `lynx.Config`（含 `UnmarshalKey(path string, out any) error`，缺失 key 返回 nil error 且 out 保持零值）、`lynx.NewViperConfig(v *viper.Viper) ConfigSource`（测试构造）
- Produces: `func NewFromConfig(cfg lynx.Config) (*Transport, error)` —— 段缺失或为空返回 `(nil, nil)`；返回具体类型 `*Transport` 保证调用方 `t == nil` 判定安全

- [ ] **Step 1: 写失败测试**

Create `contrib/kafka/fromconfig_test.go`:

```go
package kafka

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

func fromConfigTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return lynx.NewViperConfig(v)
}

func TestNewFromConfigMissingSection(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, "addr: \":9090\"\n"))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if tr != nil {
		t.Fatalf("expected nil transport for missing kafka section, got %+v", tr)
	}
}

func TestNewFromConfigEmptySection(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, "kafka: {}\n"))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if tr != nil {
		t.Fatalf("expected nil transport for empty kafka section, got %+v", tr)
	}
}

func TestNewFromConfigValid(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, `
kafka:
  hello:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_hello]
    consumer:
      group_id: consumer_hello
    producer:
      required_acks: -1
`))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport for valid kafka section")
	}
	topics := tr.Topics()
	if len(topics) != 1 || topics[0] != "hello" {
		t.Fatalf("unexpected topics: %v", topics)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd contrib/kafka && go test -run TestNewFromConfig -v .`
Expected: 编译失败 `undefined: NewFromConfig`

- [ ] **Step 3: 实现**

Create `contrib/kafka/fromconfig.go`:

```go
package kafka

import "github.com/lynx-go/lynx"

// NewFromConfig 从配置 "kafka" 段加载 Options 并创建 Transport。
// 段缺失或为空（无任何 topic）时返回 (nil, nil)，表示 Kafka 未启用；
// 调用方据此决定是否注册。
func NewFromConfig(cfg lynx.Config) (*Transport, error) {
	var opts Options
	if err := cfg.UnmarshalKey("kafka", &opts); err != nil {
		return nil, err
	}
	if len(opts.Topics) == 0 {
		return nil, nil
	}
	return NewTransport(opts)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd contrib/kafka && go test -race ./...`
Expected: 全部 PASS（含现有 transport_test.go 的测试）

- [ ] **Step 5: 提交**

```bash
git add contrib/kafka/fromconfig.go contrib/kafka/fromconfig_test.go
git commit -m "feat(kafka): 新增 NewFromConfig 配置驱动构造函数

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: pubsub.NewFromConfig 与 Bundle

**Files:**
- Create: `contrib/pubsub/builder.go`
- Test: `contrib/pubsub/builder_test.go`
- Modify: `contrib/pubsub/go.mod`（`go mod tidy` 将 viper 从 indirect 提升为 direct）

**Interfaces:**
- Consumes: `lynx.Config`、`lynx.NewViperConfig`；本包已有 `Broker`/`Transport`/`NewBroker`/`NewMemoryTransport`/`RouteKey`；测试复用 `broker_test.go` 的 `newFakeTransport`（同包）
- Produces:
  - `type Bundle struct { Broker Broker; Transports []Transport }`
  - `func (b *Bundle) Components() []lynx.Component` —— 顺序：Transports 全部在前、Broker 最后
  - `func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (*Bundle, error)`

- [ ] **Step 1: 写失败测试**

Create `contrib/pubsub/builder_test.go`:

```go
package pubsub

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

func builderTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return lynx.NewViperConfig(v)
}

// TestNewFromConfigRoutesApplied 验证显式路由与 key 别名生效。
func TestNewFromConfigRoutesApplied(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
      key: hello
    notify:
      transport: memory
      key: user_notify
`), map[string]Transport{"kafka": kafkaT, "memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b.Broker == nil {
		t.Fatal("expected non-nil broker")
	}
	if err := b.Broker.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
	if err := b.Broker.Publish(t.Context(), "notify", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish notify: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "user_notify" {
		t.Fatalf("notify published to %v, want [user_notify]", got)
	}
}

// TestNewFromConfigUnknownTransport 验证未知 transport 标识报错。
func TestNewFromConfigUnknownTransport(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: redis
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "redis"`) {
		t.Fatalf("expected unknown transport error, got %v", err)
	}
}

// TestNewFromConfigKafkaDisabledRouteError 验证 kafka 未启用时路由引用 kafka 报错。
func TestNewFromConfigKafkaDisabledRouteError(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "kafka"`) {
		t.Fatalf("expected unknown transport error for disabled kafka, got %v", err)
	}
}

// TestNewFromConfigDefaultMemory 验证无显式路由时未声明 topic 回退内置内存。
func TestNewFromConfigDefaultMemory(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"), map[string]Transport{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(b.Transports) != 1 {
		t.Fatalf("expected 1 built-in memory transport, got %d", len(b.Transports))
	}
	if _, ok := b.Transports[0].(*MemoryTransport); !ok {
		t.Fatalf("expected built-in *MemoryTransport, got %T", b.Transports[0])
	}
	if err := b.Broker.Publish(t.Context(), "anything", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish to default memory: %v", err)
	}
}

// TestNewFromConfigProvidedMemory 验证调用方提供的 memory 被复用，不重复创建。
func TestNewFromConfigProvidedMemory(t *testing.T) {
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(b.Transports) != 1 || b.Transports[0] != memT {
		t.Fatalf("expected provided memory transport reused, got %v", b.Transports)
	}
	if err := b.Broker.Publish(t.Context(), "anything", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish to provided memory: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "anything" {
		t.Fatalf("published to %v, want [anything]", got)
	}
}

// TestNewFromConfigNilEntrySkipped 验证字面 nil 条目被防御性跳过。
func TestNewFromConfigNilEntrySkipped(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": nil})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(b.Transports) != 1 {
		t.Fatalf("expected only built-in memory transport, got %d", len(b.Transports))
	}
}

// TestNewFromConfigComponentsOrder 验证 Components() 顺序稳定（transports 前、broker 后）。
func TestNewFromConfigComponentsOrder(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	comps := b.Components()
	if len(comps) != len(b.Transports)+1 {
		t.Fatalf("Components len %d, want %d", len(comps), len(b.Transports)+1)
	}
	for i, tr := range b.Transports {
		if comps[i] != tr {
			t.Fatalf("Components[%d] = %v, want transport %v", i, comps[i], tr)
		}
	}
	if comps[len(comps)-1] != b.Broker {
		t.Fatalf("last component = %v, want broker", comps[len(comps)-1])
	}
}
```

注意：`newFakeTransport` 定义在 `broker_test.go` 中，同包测试可直接复用。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd contrib/pubsub && go mod tidy && go test -run TestNewFromConfig -v .`
Expected: 编译失败 `undefined: NewFromConfig`（`go mod tidy` 会把 `github.com/spf13/viper` 从 indirect 提升为 direct，若 go.mod 发生变化一并提交）

- [ ] **Step 3: 实现**

Create `contrib/pubsub/builder.go`:

```go
package pubsub

import (
	"fmt"

	"github.com/lynx-go/lynx"
)

// Bundle 是配置驱动装配的结果：Broker 与需要随应用注册的 Transports。
type Bundle struct {
	Broker     Broker
	Transports []Transport
}

// Components 返回应注册的全部组件（Transports + Broker），供 app.Register 使用。
func (b *Bundle) Components() []lynx.Component {
	comps := make([]lynx.Component, 0, len(b.Transports)+1)
	for _, t := range b.Transports {
		comps = append(comps, t)
	}
	return append(comps, b.Broker)
}

// NewFromConfig 从配置装配消息组件：
//   - "pubsub" 段 routes（逻辑 topic → {transport, key}）逐条应用 RouteKey，
//     引用未提供的 transport 标识时报错；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport 兼作默认回退；未提供时内置创建一个
//     内存 Transport 作为默认回退并纳入 Transports；
//   - map 中的字面 nil 值条目被防御性跳过；具体类型 nil 指针赋给接口
//     形成的 typed nil 无法在此检测，调用方须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (*Bundle, error) {
	var routesCfg struct {
		Routes map[string]struct {
			Transport string
			Key       string
		}
	}
	if err := cfg.UnmarshalKey("pubsub", &routesCfg); err != nil {
		return nil, err
	}

	// 默认回退：优先复用调用方提供的 "memory"，否则内置创建。
	memT, hasMemory := transports["memory"]
	if !hasMemory || memT == nil {
		memT = NewMemoryTransport()
	}
	opts := Options{DefaultTransport: memT}

	// 自动路由 transports 与注册列表；字面 nil 防御性跳过；memory 仅作
	// 默认回退（Topics() 为 nil），不重复进入自动路由表。
	registered := make([]Transport, 0, len(transports)+1)
	for name, t := range transports {
		if t == nil {
			continue
		}
		if name == "memory" {
			registered = append(registered, t)
			continue
		}
		opts.Transports = append(opts.Transports, t)
		registered = append(registered, t)
	}
	if !hasMemory || memT == nil {
		registered = append(registered, memT)
	}

	// 路由解析表：memory 标识始终可用（未提供时指向内置默认）。
	resolve := make(map[string]Transport, len(transports)+1)
	for name, t := range transports {
		if t != nil {
			resolve[name] = t
		}
	}
	if _, ok := resolve["memory"]; !ok {
		resolve["memory"] = memT
	}

	broker := NewBroker(opts)
	for topic, route := range routesCfg.Routes {
		t, ok := resolve[route.Transport]
		if !ok {
			return nil, fmt.Errorf("pubsub: route %q references unknown transport %q", topic, route.Transport)
		}
		if route.Key == "" {
			route.Key = topic
		}
		broker.RouteKey(topic, t, route.Key)
	}
	return &Bundle{Broker: broker, Transports: registered}, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd contrib/pubsub && go test -race ./...`
Expected: 全部 PASS（含现有 broker/memory/marshaller 测试）

- [ ] **Step 5: 提交**

```bash
git add contrib/pubsub/builder.go contrib/pubsub/builder_test.go contrib/pubsub/go.mod contrib/pubsub/go.sum
git commit -m "feat(pubsub): 新增 NewFromConfig/Bundle 配置驱动装配

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: 更新 pubsub 示例（main.go + README）

**Files:**
- Modify: `_examples/pubsub/main.go:23-69`（替换装配段）
- Modify: `_examples/pubsub/README.md:19-28`（关键代码点）

**Interfaces:**
- Consumes: Task 1 `kafka.NewFromConfig`、Task 2 `pubsub.NewFromConfig`/`Bundle.Components()`

- [ ] **Step 1: 替换 main.go 装配段**

把 `_examples/pubsub/main.go` 的 23-69 行（从 `// kafka 配置从 config.yaml...` 到 `app.Register(pubsub.NewRouter(...))`）替换为：

```go
	// kafka transport 由 kafka.NewFromConfig 从 config.yaml 的 kafka 段加载
	//（--config 指定路径）；段缺失/为空时返回 nil，表示 Kafka 未启用。
	kafkaT, err := kafka.NewFromConfig(app.Config())
	if err != nil {
		return err
	}
	transports := map[string]pubsub.Transport{}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	// pubsub.NewFromConfig 从 config.yaml 的 pubsub 段加载显式路由并应用
	// RouteKey（逻辑 topic → {transport, key}）；内置内存 Transport 作为
	// 默认回退。路由引用未知 transport 标识（或 kafka 未启用仍引用
	// kafka）时构建期报错，避免路由表静默失真。
	bundle, err := pubsub.NewFromConfig(app.Config(), transports)
	if err != nil {
		return err
	}
	app.Register(bundle.Components()...)
	app.Register(pubsub.NewRouter(bundle.Broker, []pubsub.Handler{&helloHandler{}, &notifyHandler{}}))
```

同时从 import 块（第 5 行）删除 `"fmt"`（装配段不再使用）。

- [ ] **Step 2: 构建验证**

Run: `cd _examples && go build ./...`
Expected: 编译成功，无错误

- [ ] **Step 3: 更新 README 关键代码点**

把 `_examples/pubsub/README.md` 的 `## 关键代码点` 一节（19-28 行）替换为：

```markdown
## 关键代码点

- `config.yaml` `kafka` 段：逻辑 topic `hello` 的 brokers、物理 topics `[topic_hello]`、consumer（group `consumer_hello`，3 实例）与 producer 配置。
- `config.yaml` `pubsub` 段：显式路由表——逻辑 topic（业务事件名）→ `{transport, key}`，覆盖自动路由。`transport` 为后端标识（`kafka`/`memory`）；`key` 是调用 transport 时的主题名，对 kafka 即 kafka 段配置的逻辑 key（如 `hello→hello`），可与逻辑名不同（如 `notify→user_notify`）。未在此列出的 topic 按自动路由/默认回退处理。
- `main.go` `kafka.NewFromConfig`：从 `app.Config()` 的 `kafka` 段加载配置创建 Kafka Transport（订阅按消费组 × 物理 topic × 实例数展开）；段缺失/为空时返回 nil，Kafka 未启用。
- `main.go` `pubsub.NewFromConfig`：装配消息组件——加载 `pubsub` 段路由表并逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错）；传入 transports 自动路由；未提供 `memory` 时内置创建内存 Transport 作为默认回退。
- `main.go` `bundle.Components()`：一次性注册全部消息组件（transports + broker）。
- `main.go` `pubsub.NewRouter`：将 `helloHandler`、`notifyHandler` 缓冲订阅到 Broker。
- `main.go` `/hello`、`/notify` HTTP 端点（`:7071`）分别发布 JSON 事件，带 UUID message key。
- `main.go` `helloHandler`/`notifyHandler`：实现 `pubsub.Handler`，分别消费 `hello`（Kafka）与 `notify`（内存）事件并记录 payload。
```

- [ ] **Step 4: 提交**

```bash
git add _examples/pubsub/main.go _examples/pubsub/README.md
git commit -m "refactor(examples): pubsub 示例改用配置驱动装配

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: 更新 docs/04-component-system.md

**Files:**
- Modify: `docs/04-component-system.md:197-297`（pubsub 与 kafka 两节）

**Interfaces:**
- Consumes: Task 1/2/3 的最终 API 形态（`kafka.NewFromConfig`、`pubsub.NewFromConfig`、`Bundle`、`bundle.Components()`）
- 注意：该文件工作区已有未提交修改，编辑须基于当前工作区内容（先 `git diff` 确认现状），只改动下述两处

- [ ] **Step 1: 更新 pubsub 节用法示例**

把 `docs/04-component-system.md` 第 205-217 行的"用法（取自 `_examples/pubsub/main.go`）"代码块替换为：

```go
kafkaT, err := kafka.NewFromConfig(app.Config()) // nil = kafka 段缺失，未启用
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
app.Register(pubsub.NewRouter(b.Broker, []pubsub.Handler{&helloHandler{}}))
```

- [ ] **Step 2: 在 pubsub 节概念列表（201-203 行）后新增配置驱动装配说明**

在 `Router` 条目之后插入：

```markdown
- `Bundle`/`NewFromConfig`：配置驱动装配——`pubsub.NewFromConfig(cfg, transports)` 从配置 `pubsub` 段加载显式路由并逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错），非 nil 的传入 transports 参与自动路由；未提供 `memory` 标识时内置创建一个内存 Transport 作为默认回退。返回 `*Bundle`（`Broker` + 待注册 `Transports`），`bundle.Components()` 一次性返回应注册的全部组件。`kafka.NewFromConfig(cfg)` 配套加载 `kafka` 段创建 Transport，段缺失/为空返回 `(nil, nil)`（未启用），调用方过滤后再放入 transports 表。
```

- [ ] **Step 3: 更新 kafka 节"加载与注册"示例**

把第 270-280 行的代码块替换为：

```go
kafkaT, err := kafka.NewFromConfig(app.Config()) // 段缺失/为空返回 nil，未启用
if err != nil {
	return err
}
if kafkaT != nil {
	app.Register(kafkaT)
}
```

并在该节描述（247 行）中把"再交给 `kafka.NewTransport` 构造"改为"再由 `kafka.NewFromConfig` 构造（`NewTransport` 仍可用代码直接构造）"。

- [ ] **Step 4: 检查文档无残留旧示例**

Run: `grep -n "UnmarshalKey(\"kafka\"" docs/04-component-system.md`
Expected: 无输出（旧加载模式示例已全部移除；如仍有残留则替换为 `kafka.NewFromConfig` 形态）

- [ ] **Step 5: 提交**

```bash
git add docs/04-component-system.md
git commit -m "docs: 组件系统文档更新 pubsub/kafka 配置驱动装配用法

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: 全量回归验证

**Files:** 无

- [ ] **Step 1: 各模块全量测试**

Run:
```bash
cd contrib/kafka && go test -race ./... && cd ../pubsub && go test -race ./... && cd ../../_examples && go build ./... && cd ..
```
Expected: 全部 PASS；示例构建成功

- [ ] **Step 2: 运行示例冒烟（可选，需本地 Kafka）**

Run: `cd _examples/pubsub && go run . --config=config.yaml`
Expected: 应用启动，健康检查 `/healthz/readiness` 正常；无 Kafka 环境时删去 config.yaml 的 `kafka` 段再跑，应纯内存启动成功

---

### Task 6: pubsub 示例改造为 wire+boot 方式

用户需求：把 `_examples/pubsub` 改为 Wire 依赖注入 + boot 引导的方式（对齐 `_examples/boot` 的结构），保留 `/hello`、`/notify` 端点与两个 handler，配置仍全部来自 config.yaml。

**Files:**
- Rewrite: `_examples/pubsub/main.go`
- Create: `_examples/pubsub/provides.go`
- Create: `_examples/pubsub/wire.go`
- Create: `_examples/pubsub/handlers.go`（从 main.go 移出 handler 定义）
- Create: `_examples/pubsub/wire_gen.go`（由 wire 工具生成，随代码提交）
- Modify: `_examples/pubsub/README.md`（运行说明加 wire 生成步骤；关键代码点改为 provider 视角）

**Interfaces:**
- Consumes: `kafka.NewFromConfig(cfg lynx.Config) (*Transport, error)`（nil=未启用）、`pubsub.NewFromConfig(cfg lynx.Config, transports map[string]Transport) (*Bundle, error)`、`Bundle.Broker`、`Bundle.Components()`、`pubsub.NewRouter`、`boot.New`/`Bootstrap.Bind`、`lynx.NewBuilder`、`zap.MustNewLogger`
- Produces: `wireBootstrap(app lynx.App) (*boot.Bootstrap, func(), error)` 注入器；`ProviderSet`（provides.go）；wire_gen.go

- [ ] **Step 1: 重写 main.go**

`_examples/pubsub/main.go` 整体替换为：

```go
package main

import (
	"context"
	"os"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/samber/lo"
)

func main() {
	builder := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))

		// Wire 依赖注入生成 bootstrap（provides.go 的 ProviderSet 定义
		// kafka/pubsub/http 各组件 provider，配置全部来自 config.yaml）。
		bootstrap, cleanup, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		app.OnStop(func(ctx context.Context) error {
			cleanup()
			return nil
		})
		bootstrap.Bind(app)
		return nil
	},
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)
	builder.Run()
}
```

- [ ] **Step 2: 新建 provides.go**

`_examples/pubsub/provides.go`（注意 `//go:generate wire` 在文件顶部）：

```go
package main

import (
	gohttp "net/http"

	"github.com/google/uuid"
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/lynx-go/lynx/contrib/kafka"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/server/http"
	"github.com/lynx-go/x/log"
)

//go:generate wire

// ProviderSet 是 pubsub 示例的 Wire 依赖集合：kafka/pubsub 配置驱动
// 构造函数直接作为 provider（纯函数，Wire 按类型图注入）。
var ProviderSet = wire.NewSet(
	boot.New,
	NewConfig,
	ProvideKafkaTransport,
	ProvideBundle,
	ProvideHandlers,
	ProvideRouter,
	NewHttpServer,
	NewComponents,
	NewOnStarts,
	NewOnStops,
)

// NewConfig 提供应用配置（kafka/pubsub 段的读取源）。
func NewConfig(app lynx.App) lynx.Config {
	return app.Config()
}

// ProvideKafkaTransport 从配置 kafka 段创建 Transport；段缺失/为空时
// kafka.NewFromConfig 返回 nil（未启用），Wire 注入 nil 指针。
func ProvideKafkaTransport(cfg lynx.Config) (*kafka.Transport, error) {
	t, err := kafka.NewFromConfig(cfg)
	if t == nil {
		return nil, err
	}
	return t, err
}

// ProvideBundle 装配消息组件：pubsub.NewFromConfig 从配置 pubsub 段加载
// 显式路由，内置内存 Transport 兜底；kafka 未启用时过滤。
func ProvideBundle(cfg lynx.Config, kafkaT *kafka.Transport) (*pubsub.Bundle, error) {
	transports := map[string]pubsub.Transport{}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	return pubsub.NewFromConfig(cfg, transports)
}

// ProvideHandlers 提供事件处理器集合。
func ProvideHandlers() []pubsub.Handler {
	return []pubsub.Handler{&helloHandler{}, &notifyHandler{}}
}

// ProvideRouter 将处理器缓冲订阅到 Broker。
func ProvideRouter(bundle *pubsub.Bundle, handlers []pubsub.Handler) *pubsub.Router {
	return pubsub.NewRouter(bundle.Broker, handlers)
}

// NewHttpServer 构建 HTTP 服务：/hello 与 /notify 端点发布事件。
func NewHttpServer(bundle *pubsub.Bundle) *http.Server {
	mux := gohttp.NewServeMux()
	mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := bundle.Broker.Publish(request.Context(), "hello",
			pubsub.MustJSONMessage(map[string]any{"message": "hello"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			log.ErrorContext(request.Context(), "failed to publish", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	mux.HandleFunc("/notify", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := bundle.Broker.Publish(request.Context(), "notify",
			pubsub.MustJSONMessage(map[string]any{"message": "notify"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			log.ErrorContext(request.Context(), "failed to publish", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	return http.NewServer(mux, http.WithAddr(":7071"))
}

// NewComponents 聚合全部组件供 bootstrap 注册。
func NewComponents(bundle *pubsub.Bundle, router *pubsub.Router, hs *http.Server) []lynx.Component {
	return append(bundle.Components(), router, hs)
}

func NewOnStarts() boot.OnStartHooks { return nil }
func NewOnStops() boot.OnStopHooks  { return nil }
```

- [ ] **Step 3: 新建 wire.go**

`_examples/pubsub/wire.go`（build tag 与 `_examples/boot/wire.go` 一致）：

```go
//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.
package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
)

func wireBootstrap(app lynx.App) (*boot.Bootstrap, func(), error) {
	panic(wire.Build(ProviderSet))
}
```

- [ ] **Step 4: 新建 handlers.go**

`_examples/pubsub/handlers.go`（从旧 main.go 移入，注释与实现原样保留）：

```go
package main

import (
	"context"

	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
)

// helloHandler 消费 hello 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到 kafka transport）。
type helloHandler struct{}

func (h *helloHandler) EventName() string   { return "hello" }
func (h *helloHandler) HandlerName() string { return "helloHandler" }

func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		log.InfoContext(ctx, "hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)

// notifyHandler 订阅 notify 逻辑 topic（config.yaml 的 pubsub.routes 显式
// 路由到内存 transport，不经过 Kafka）。
type notifyHandler struct{}

func (h *notifyHandler) EventName() string   { return "notify" }
func (h *notifyHandler) HandlerName() string { return "notifyHandler" }

func (h *notifyHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		log.InfoContext(ctx, "notify event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(notifyHandler)
```

- [ ] **Step 5: 生成 wire 代码并构建验证**

Run: `cd _examples/pubsub && wire && go build ./... && go vet ./...`
Expected: wire 生成 `wire_gen.go`（无错误输出）；build/vet 通过。若 wire 不在 PATH，先 `go install github.com/google/wire/cmd/wire@latest`。
注意：wire 生成产物 `wire_gen.go` 中 injector 签名必须为 `func wireBootstrap(app lynx.App) (*boot.Bootstrap, func(), error)`（与 wire.go stub 一致）。

- [ ] **Step 6: 更新 README**

`_examples/pubsub/README.md`：
- `## 运行` 一节在 `go run .` 前加 `go generate`（生成 wire 依赖图）步骤：

```markdown
## 运行

```bash
go generate .   # Wire 生成依赖图（wire_gen.go）
go run . --config=config.yaml
# 另开终端触发发布（hello 走 Kafka；notify 走内存 transport）
curl http://127.0.0.1:7071/hello
curl http://127.0.0.1:7071/notify
```
```

- `## 关键代码点` 一节改为 provider 视角，替换为：

```markdown
## 关键代码点

- `provides.go` `ProviderSet`：Wire 依赖集合——`kafka.NewFromConfig`/`pubsub.NewFromConfig` 等配置驱动构造函数直接作为 provider。
- `provides.go` `ProvideKafkaTransport`：从 `app.Config()` 的 `kafka` 段加载配置创建 Kafka Transport（订阅按消费组 × 物理 topic × 实例数展开）；段缺失/为空时返回 nil，Wire 注入 nil 指针、`ProvideBundle` 过滤，kafka 未启用。
- `provides.go` `ProvideBundle`：装配消息组件——加载 `pubsub` 段路由表逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错）；内置内存 Transport 作为默认回退。
- `provides.go` `NewComponents`：聚合 `bundle.Components()`（transports + broker）、Router、HTTP Server 供 `boot.Bootstrap.Bind` 注册。
- `wire.go`：`//go:build wireinject` 注入器 stub，`go generate` 生成 `wire_gen.go`。
- `handlers.go` `helloHandler`/`notifyHandler`：实现 `pubsub.Handler`，分别消费 `hello`（Kafka）与 `notify`（内存）事件并记录 payload。
- `main.go` `/hello`、`/notify` HTTP 端点（`:7071`）分别发布 JSON 事件，带 UUID message key。
```

- [ ] **Step 7: 提交**

```bash
git add _examples/pubsub/
git commit -m "refactor(examples): pubsub 示例改为 wire+boot 依赖注入方式

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: 最终审查补缺（测试 + 确定性排序）

**Files:**
- Modify: `contrib/pubsub/builder.go`（按名字排序遍历 transports，注册顺序确定性）
- Test: `contrib/pubsub/builder_test.go`（route.Key 缺省测试、显式路由→内置 memory 测试、ComponentsOrder 精确断言）
- Test: `contrib/kafka/fromconfig_test.go`（UnmarshalKey 失败错误路径测试）

**Interfaces:**
- Consumes: Task 2 的 `NewFromConfig`/`Bundle` 现有行为；`newFakeTransport`（broker_test.go）

- [ ] **Step 1: 写失败测试**

在 `contrib/pubsub/builder_test.go` 末尾追加两个测试，并更新 ComponentsOrder：

```go
// TestNewFromConfigRouteKeyDefaultsToTopic 验证路由未指定 key 时缺省为逻辑 topic 名。
func TestNewFromConfigRouteKeyDefaultsToTopic(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
`), map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Broker.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
}

// TestNewFromConfigRouteToBuiltinMemory 验证显式路由指向未提供的 memory
// 标识时解析到内置内存 Transport（不报未知标识错误）。
func TestNewFromConfigRouteToBuiltinMemory(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    notify:
      transport: memory
`), map[string]Transport{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Broker.Publish(t.Context(), "notify", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish notify via built-in memory: %v", err)
	}
}
```

把现有 `TestNewFromConfigComponentsOrder` 更新为精确顺序断言（确定性排序后：名字排序 kafka 在前、内置 memory 在后、broker 最后）：

```go
// TestNewFromConfigComponentsOrder 验证 Components() 顺序确定性：按名字
// 排序的 transports（内置 memory 最后）在前、Broker 最后。
func TestNewFromConfigComponentsOrder(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	want := []lynx.Component{kafkaT, b.Transports[1], b.Broker}
	comps := b.Components()
	if len(comps) != 3 {
		t.Fatalf("Components len %d, want 3", len(comps))
	}
	for i, w := range want {
		if comps[i] != w {
			t.Fatalf("Components[%d] = %v, want %v", i, comps[i], w)
		}
	}
}
```

在 `contrib/kafka/fromconfig_test.go` 末尾追加：

```go
// TestNewFromConfigInvalidSection 验证 kafka 段类型非法时返回错误。
func TestNewFromConfigInvalidSection(t *testing.T) {
	tr, err := NewFromConfig(fromConfigTestConfig(t, `
kafka:
  hello:
    brokers: 42
`))
	if err == nil {
		t.Fatal("expected error for invalid kafka section (brokers must be []string)")
	}
	if tr != nil {
		t.Fatalf("expected nil transport on error, got %+v", tr)
	}
}
```

- [ ] **Step 2: 跑测试确认新测试失败**

Run: `cd contrib/pubsub && go test -run 'TestNewFromConfigRouteKeyDefaultsToTopic|TestNewFromConfigRouteToBuiltinMemory|TestNewFromConfigComponentsOrder' -v . && cd ../kafka && go test -run TestNewFromConfigInvalidSection -v .`
Expected: RouteKeyDefaultsToTopic 失败（当前无 key 缺省：route key 为空字符串 → resolve 时 key=="" → 回退 topic 名——若实现已缺省正确则此测试通过，需要确认缺省行为是否已被 route.Key=="" 分支覆盖；RouteToBuiltinMemory 失败（当前 resolve 表无 memory → 报未知标识）；ComponentsOrder 断言长度不符失败；InvalidSection 通过或失败取决于 UnmarshalKey 行为，如实记录）
说明：若个别新测试首跑即通过（如 RouteKeyDefaultsToTopic——builder.go 已有 `if route.Key == "" { route.Key = topic }` 分支），如实记录为"已通过（覆盖既有实现）"，不强行制造失败。

- [ ] **Step 3: 实现确定性排序**

修改 `contrib/pubsub/builder.go`：imports 加 `"sort"`；transports 遍历改为按名字排序：

```go
	// 自动路由 transports 与注册列表；字面 nil 防御性跳过；memory 仅作
	// 默认回退（Topics() 为 nil），不重复进入自动路由表。按名字排序遍历，
	// 保证注册顺序确定（组件启动顺序可复现）。
	names := make([]string, 0, len(transports))
	for name := range transports {
		names = append(names, name)
	}
	sort.Strings(names)
	registered := make([]Transport, 0, len(transports)+1)
	for _, name := range names {
		t := transports[name]
		if t == nil {
			continue
		}
		if name == "memory" {
			registered = append(registered, t)
			continue
		}
		opts.Transports = append(opts.Transports, t)
		registered = append(registered, t)
	}
	if !hasMemory || memT == nil {
		registered = append(registered, memT)
	}
```

- [ ] **Step 4: 跑测试确认全部通过**

Run: `cd contrib/pubsub && go test -race ./... && cd ../kafka && go test -race ./...`
Expected: 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add contrib/pubsub/builder.go contrib/pubsub/builder_test.go contrib/kafka/fromconfig_test.go
git commit -m "test(pubsub,kafka): 审查补缺——key 缺省/内置 memory 路由测试 + 确定性排序

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 8: 移除 Bundle——transports 一律显式

用户决策（2026-08-06）：pubsub 概念过多，移除 `Bundle`。设计依据（见 spec 修订记录）：Bundle 的必要性完全源于"隐式内置 memory"——改为显式后 `NewFromConfig` 返回 `Broker` 单值（Wire 天然友好），kafka 与 memory 对称。`transports["memory"]`（提供且非 nil 时）兼作默认回退的文档约定**保留**；不再内置创建任何 transport。

**Files:**
- Rewrite: `contrib/pubsub/builder.go`（删 Bundle/Components，NewFromConfig → `(Broker, error)`）
- Rewrite: `contrib/pubsub/builder_test.go`（按 spec 测试计划更新）
- Modify: `_examples/pubsub/provides.go`（NewBundle → NewMemoryTransport + NewBroker；Router/HttpServer/Components 改收 `pubsub.Broker`）
- Regenerate: `_examples/pubsub/wire_gen.go`（wire 工具）
- Modify: `_examples/pubsub/README.md`、`docs/04-component-system.md`（Bundle 提及同步）

**Interfaces:**
- Consumes: `lynx.Config`、`NewBroker`、`RouteKey`、`newFakeTransport`；`kafka.NewFromConfig`（不变）
- Produces: `func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (Broker, error)`（无 Bundle）

- [ ] **Step 1: 重写 builder.go（TDD：先写测试）**

`contrib/pubsub/builder.go` 整体替换为（删除 Bundle/Components，`"sort"` import 保留用于确定序）：

```go
package pubsub

import (
	"fmt"
	"sort"

	"github.com/lynx-go/lynx"
)

// NewFromConfig 从配置装配 Broker：
//   - "pubsub" 段 routes（逻辑 topic → {transport, key}）逐条应用 RouteKey，
//     引用未提供的 transport 标识时报错；
//   - 传入 transports 的非 nil 值参与自动路由；
//   - 标识 "memory" 的 transport（提供且非 nil 时）兼作默认回退——未路由
//     的 topic 走它；不提供则无默认回退，未路由 topic 发布报错；
//   - 不创建任何 transport：kafka 与 memory 一律由调用方创建并注册
//     （生命周期归属应用）；
//   - map 中的字面 nil 值条目被防御性跳过；kafka 未启用的过滤由调用方
//     完成（示例 `if kafkaT != nil` 写法）。注意：具体类型 nil 指针赋给
//     Transport 接口（typed nil）无法在此检测，调用方必须过滤后再放入 map。
func NewFromConfig(cfg lynx.Config, transports map[string]Transport) (Broker, error) {
	var routesCfg struct {
		Routes map[string]struct {
			Transport string
			Key       string
		}
	}
	if err := cfg.UnmarshalKey("pubsub", &routesCfg); err != nil {
		return nil, err
	}

	// 自动路由 transports 按名字排序遍历，保证多个 transport 声明同一
	// topic 时 Init 的冲突报错顺序确定（启动行为可复现）。
	opts := Options{}
	names := make([]string, 0, len(transports))
	for name := range transports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := transports[name]
		if t == nil {
			continue // 字面 nil 防御性跳过
		}
		opts.Transports = append(opts.Transports, t)
		if name == "memory" {
			opts.DefaultTransport = t
		}
	}

	broker := NewBroker(opts)
	for topic, route := range routesCfg.Routes {
		t, ok := transports[route.Transport]
		if !ok || t == nil {
			return nil, fmt.Errorf("pubsub: route %q references unknown transport %q", topic, route.Transport)
		}
		if route.Key == "" {
			route.Key = topic
		}
		broker.RouteKey(topic, t, route.Key)
	}
	return broker, nil
}
```

- [ ] **Step 2: 重写 builder_test.go**

`contrib/pubsub/builder_test.go` 整体替换为：

```go
package pubsub

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

func builderTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return lynx.NewViperConfig(v)
}

// TestNewFromConfigRoutesApplied 验证显式路由与 key 别名生效。
func TestNewFromConfigRoutesApplied(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
      key: hello
    notify:
      transport: memory
      key: user_notify
`), map[string]Transport{"kafka": kafkaT, "memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil broker")
	}
	if err := b.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
	if err := b.Publish(t.Context(), "notify", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish notify: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "user_notify" {
		t.Fatalf("notify published to %v, want [user_notify]", got)
	}
}

// TestNewFromConfigRouteKeyDefaultsToTopic 验证路由未指定 key 时缺省为逻辑
// topic 名（fake 不声明 topic，显式路由是唯一路径，独占验证缺省分支）。
func TestNewFromConfigRouteKeyDefaultsToTopic(t *testing.T) {
	kafkaT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
`), map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Publish(t.Context(), "hello", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish hello: %v", err)
	}
	if got := kafkaT.publishedTopics(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hello published to %v, want [hello]", got)
	}
}

// TestNewFromConfigUnknownTransport 验证未知 transport 标识报错。
func TestNewFromConfigUnknownTransport(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: redis
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "redis"`) {
		t.Fatalf("expected unknown transport error, got %v", err)
	}
}

// TestNewFromConfigKafkaDisabledRouteError 验证 kafka 未启用时路由引用 kafka 报错。
func TestNewFromConfigKafkaDisabledRouteError(t *testing.T) {
	_, err := NewFromConfig(builderTestConfig(t, `
pubsub:
  routes:
    hello:
      transport: kafka
`), map[string]Transport{})
	if err == nil || !strings.Contains(err.Error(), `route "hello" references unknown transport "kafka"`) {
		t.Fatalf("expected unknown transport error for disabled kafka, got %v", err)
	}
}

// TestNewFromConfigNilEntrySkipped 验证字面 nil 条目被防御性跳过。
func TestNewFromConfigNilEntrySkipped(t *testing.T) {
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": nil})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil broker")
	}
}

// TestNewFromConfigMemoryFallback 验证提供 memory 时未路由 topic 回退到它。
func TestNewFromConfigMemoryFallback(t *testing.T) {
	memT := newFakeTransport()
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"memory": memT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Publish(t.Context(), "anything", NewMessage([]byte("x"))); err != nil {
		t.Fatalf("publish to memory fallback: %v", err)
	}
	if got := memT.publishedTopics(); len(got) != 1 || got[0] != "anything" {
		t.Fatalf("published to %v, want [anything]", got)
	}
}

// TestNewFromConfigNoMemoryNoFallback 验证未提供 memory 时未路由 topic 发布报错。
func TestNewFromConfigNoMemoryNoFallback(t *testing.T) {
	kafkaT := newFakeTransport("hello")
	b, err := NewFromConfig(builderTestConfig(t, "addr: \":9090\"\n"),
		map[string]Transport{"kafka": kafkaT})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if err := b.Publish(t.Context(), "unrouted", NewMessage([]byte("x"))); err == nil {
		t.Fatal("expected error publishing unrouted topic without default transport")
	}
}
```

删除的旧测试：`TestNewFromConfigDefaultMemory`、`TestNewFromConfigProvidedMemory`、`TestNewFromConfigRouteToBuiltinMemory`、`TestNewFromConfigComponentsOrder`、`TestNewFromConfigComponentsOrderMulti`。

- [ ] **Step 3: 跑测试确认通过**

Run: `cd contrib/pubsub && go test -race -run TestNewFromConfig -v .`
Expected: 7 个测试全部 PASS（TDD 说明：本任务先实现后测试不适用——Step 1 与 Step 2 是同一提交的配套重写，首跑即全绿属于"重写验证"而非"新行为测试"，如实记录即可）

- [ ] **Step 4: 更新示例 provides.go**

`_examples/pubsub/provides.go` 修改：
- ProviderSet 中 `NewBundle` 替换为 `NewMemoryTransport, NewBroker`
- 删 `NewBundle` 函数；新增 `NewMemoryTransport`；`NewBroker` 接收 `cfg lynx.Config, kafkaT *kafka.Transport, memT *pubsub.MemoryTransport` 返回 `(pubsub.Broker, error)`
- `NewRouter` 改收 `broker pubsub.Broker`（不再收 `*pubsub.Bundle`）
- `NewHttpServer` 改收 `broker pubsub.Broker`，内部 `bundle.Broker.Publish` → `broker.Publish`
- `NewComponents` 改收 `memT *pubsub.MemoryTransport, kafkaT *kafka.Transport, broker pubsub.Broker, router *pubsub.Router, hs *http.Server`

替换后的相关函数：

```go
var ProviderSet = wire.NewSet(
	boot.New,
	NewConfig,
	NewKafkaTransport,
	NewMemoryTransport,
	NewBroker,
	NewHandlers,
	NewRouter,
	NewHttpServer,
	NewComponents,
	NewComponentBuilders,
	NewOnStarts,
	NewOnStops,
)

// NewMemoryTransport 提供进程内 Transport（默认回退与本地开发用）。
func NewMemoryTransport() *pubsub.MemoryTransport {
	return pubsub.NewMemoryTransport()
}

// NewBroker 装配消息组件：pubsub.NewFromConfig 从配置 pubsub 段加载
// 显式路由，memory 兼作默认回退；kafka 未启用时过滤。
func NewBroker(cfg lynx.Config, kafkaT *kafka.Transport, memT *pubsub.MemoryTransport) (pubsub.Broker, error) {
	transports := map[string]pubsub.Transport{"memory": memT}
	if kafkaT != nil {
		transports["kafka"] = kafkaT
	}
	return pubsub.NewFromConfig(cfg, transports)
}

// NewRouter 将处理器缓冲订阅到 Broker。
func NewRouter(broker pubsub.Broker, handlers []pubsub.Handler) *pubsub.Router {
	return pubsub.NewRouter(broker, handlers)
}

// NewHttpServer 构建 HTTP 服务：/hello 与 /notify 端点发布事件。
func NewHttpServer(broker pubsub.Broker) *http.Server {
	mux := gohttp.NewServeMux()
	mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := broker.Publish(request.Context(), "hello",
			pubsub.MustJSONMessage(map[string]any{"message": "hello"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			log.ErrorContext(request.Context(), "failed to publish", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	mux.HandleFunc("/notify", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
		if err := broker.Publish(request.Context(), "notify",
			pubsub.MustJSONMessage(map[string]any{"message": "notify"}),
			pubsub.WithMessageKey(uuid.NewString()),
		); err != nil {
			log.ErrorContext(request.Context(), "failed to publish", err)
			writer.WriteHeader(gohttp.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	})
	return http.NewServer(mux, http.WithAddr(":7071"))
}

// NewComponents 聚合全部组件供 bootstrap 注册。
func NewComponents(memT *pubsub.MemoryTransport, kafkaT *kafka.Transport,
	broker pubsub.Broker, router *pubsub.Router, hs *http.Server) []lynx.Component {
	comps := []lynx.Component{memT}
	if kafkaT != nil {
		comps = append(comps, kafkaT)
	}
	return append(comps, broker, router, hs)
}
```

- [ ] **Step 5: 重新生成 wire 并构建验证**

Run: `cd _examples/pubsub && wire && cd ../.. && cd _examples && go build ./... && go vet ./...`
Expected: wire 生成新 `wire_gen.go`（调用 NewBroker/NewMemoryTransport，broker 以 `pubsub.Broker` 注入 Router/HttpServer/Components）；build/vet 通过

- [ ] **Step 6: 更新 README 与 docs/04**

`_examples/pubsub/README.md` `## 关键代码点` 中：
- `- `provides.go` `NewBundle`：装配消息组件——...` 改为 `- `provides.go` `NewBroker`：经 `pubsub.NewFromConfig` 装配——加载 `pubsub` 段路由表逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错）；`memory` 标识兼作默认回退。`
- 新增一条：`- `provides.go` `NewMemoryTransport`：显式创建内存 Transport（kafka 与 memory 对称，均由调用方创建并注册）。`
- `- `provides.go` `NewComponents`：聚合 `bundle.Components()`（transports + broker）、Router、HTTP Server 供 `boot.Bootstrap.Bind` 注册。` 改为 `- `provides.go` `NewComponents`：聚合 memT/kafkaT、Broker、Router、HTTP Server 供 `boot.Bootstrap.Bind` 注册。`

`docs/04-component-system.md` pubsub 节：
- 概念列表的 `Bundle`/`NewFromConfig` 条目改为：
  `- `NewFromConfig`：配置驱动装配——`pubsub.NewFromConfig(cfg, transports)` 从配置 `pubsub` 段加载显式路由并逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错），非 nil 的传入 transports 参与自动路由，`memory` 标识（提供时）兼作默认回退；不创建任何 transport，返回 `Broker`，transports 由调用方创建并注册。`kafka.NewFromConfig(cfg)` 配套加载 `kafka` 段创建 Transport，段缺失/为空返回 `(nil, nil)`（未启用），调用方过滤后再放入 transports 表。`
- 用法示例代码块中 `b, err := pubsub.NewFromConfig(app.Config(), transports)` / `app.Register(b.Components()...)` / `pubsub.NewRouter(b.Broker, ...)` 改为显式 memory 形态（与 spec"示例更新"小节一致）：
  ```go
  kafkaT, err := kafka.NewFromConfig(app.Config()) // nil = kafka 段缺失，未启用
  if err != nil {
  	return err
  }
  memT := pubsub.NewMemoryTransport()
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
  app.Register(pubsub.NewRouter(broker, []pubsub.Handler{&helloHandler{}}))
  ```

- [ ] **Step 7: 全量回归并提交**

Run:
```bash
cd contrib/pubsub && go test -race ./... && cd ../kafka && go test -race ./... && cd ../../_examples && go build ./... && cd ..
```
Expected: 全部 PASS；示例构建成功

```bash
git add contrib/pubsub/builder.go contrib/pubsub/builder_test.go _examples/pubsub/provides.go _examples/pubsub/wire_gen.go _examples/pubsub/README.md docs/04-component-system.md
git commit -m "refactor(pubsub): 移除 Bundle，transports 一律显式

NewFromConfig 返回 Broker 单值（Wire 天然友好）；memory 标识兼作默认
回退的文档约定保留；示例 provider 同步（NewMemoryTransport/NewBroker）

Co-Authored-By: Claude <noreply@anthropic.com>"
```
