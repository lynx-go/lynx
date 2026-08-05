# PubSub 架构重构实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 `contrib/pubsub` 与 `contrib/kafka` 从"Binder + 内部主题转发"重构为"Broker 门面 + Transport 可插拔"架构：公共 API 与 watermill 解耦（自有 `Message` 类型）、按 topic 路由到不同后端、配置驱动（`UnmarshalKey` 整表加载）。

**Architecture:** Broker 门面持有 `topic → Transport` 路由表（自动路由 + 显式 Route 覆盖 + DefaultTransport 回退，无归属则启动报错）；Transport = watermill Pub/Sub 适配 + lynx 组件（生命周期/健康检查）；kafka Transport 内部按 brokers 分组客户端，订阅按（消费组 × 物理 topic × 实例数）展开后 fan-in。消息转换只在 Broker 边界发生。

**Tech Stack:** Go 1.25、watermill v1.5.1（message / router / gochannel）、watermill-kafka/v3 v3.1.0（IBM/sarama）、viper v1.21（config）、go-viper/mapstructure/v2（`,remain` 承接动态 topic 键，已验证可行）

## Global Constraints

- Go 1.25+，所有模块 go.mod `go 1.25.0`
- 模块路径不变：`github.com/lynx-go/lynx`、`github.com/lynx-go/lynx/contrib/pubsub`、`.../contrib/kafka`
- 公共 API 禁止暴露 watermill 类型：`HandlerFunc`/`Publish`/`Subscribe` 一律使用 `pubsub.Message`（`Transport` 接口内部除外）
- 配置结构定稿：`kafka` 段 = `map[逻辑topic]`；mapstructure tag 与 yaml 键一致（`group_id`/`commit_interval`/`log_message`）；`Options.Topics map[string]TopicOptions` 使用 `mapstructure:",remain"`
- 所有导出符号有中文 GoDoc（revive exported 规则，CI 强制）
- 提交使用仓库 conventional commits 风格（feat/fix/docs/refactor + 中文描述）
- 每个任务结束运行 `gofmt -l .`（应无输出）与 `go vet ./...`
- 规格依据：`docs/superpowers/specs/2026-08-05-pubsub-redesign-design.md`（简称"设计文档"）

---

### Task 1: lynx core —— Config 接口新增 UnmarshalKey

**Files:**
- Modify: `config.go`（接口 + `viperConfig` 实现）
- Test: `config_test.go`（新建）

**Interfaces:**
- Consumes: `github.com/spf13/viper`（已有依赖）
- Produces: `lynx.Config.UnmarshalKey(path string, out any) error` —— Task 5 的 kafka 配置加载依赖它

- [ ] **Step 1: 写失败测试**

创建 `config_test.go`：

```go
package lynx

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestViperConfigUnmarshalKey(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
kafka:
  orders:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_orders]
    consumer:
      group_id: orders-group
      instances: 3
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	type consumerOptions struct {
		GroupID   string `mapstructure:"group_id"`
		Instances int    `mapstructure:"instances"`
	}
	type topicOptions struct {
		Brokers  []string         `mapstructure:"brokers"`
		Topics   []string         `mapstructure:"topics"`
		Consumer *consumerOptions `mapstructure:"consumer"`
	}
	var got struct {
		Orders topicOptions `mapstructure:"orders"`
	}
	if err := NewViperConfig(v).UnmarshalKey("kafka", &got); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	if len(got.Orders.Brokers) != 1 || got.Orders.Brokers[0] != "127.0.0.1:19092" {
		t.Fatalf("unexpected brokers: %+v", got.Orders.Brokers)
	}
	if got.Orders.Consumer == nil || got.Orders.Consumer.GroupID != "orders-group" {
		t.Fatalf("unexpected consumer: %+v", got.Orders.Consumer)
	}
	if got.Orders.Consumer.Instances != 3 {
		t.Fatalf("unexpected instances: %d", got.Orders.Consumer.Instances)
	}
}

func TestViperConfigUnmarshalKeyMissingPath(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader("addr: \":9090\"\n")); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	var got map[string]string
	if err := NewViperConfig(v).UnmarshalKey("kafka", &got); err != nil {
		t.Fatalf("UnmarshalKey on missing path: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %+v", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./... -run TestViperConfigUnmarshalKey`
Expected: 编译失败，`c.UnmarshalKey undefined (type Config has no field or method UnmarshalKey)`

- [ ] **Step 3: 实现**

修改 `config.go`：

在 `Config` 接口的 `Unmarshal` 方法后追加：

```go
	// UnmarshalKey 将 path 对应的配置子树解码到 out 指向的结构体。
	UnmarshalKey(path string, out any) error
```

在 `viperConfig` 的 `Unmarshal` 方法后追加：

```go
func (c *viperConfig) UnmarshalKey(key string, out any) error {
	return c.v.UnmarshalKey(key, out)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./... -run TestViperConfigUnmarshalKey`
Expected: `ok  	github.com/lynx-go/lynx	...`

- [ ] **Step 5: 提交**

```bash
git add config.go config_test.go
git commit -m "feat(core): Config 接口新增 UnmarshalKey 子树解码"
```

---

### Task 2: pubsub.Message 类型与 watermill 转换

**Files:**
- Create: `contrib/pubsub/message.go`
- Modify: `contrib/pubsub/broker.go`（删除 context helpers，迁移到 message.go）
- Test: `contrib/pubsub/message_test.go`（新建）

**Interfaces:**
- Consumes: `github.com/google/uuid`、`github.com/ThreeDotsLabs/watermill/message`（pubsub go.mod 已有）
- Produces:
  - `type Message struct { ID string; Key string; Headers map[string]string; Payload []byte }`
  - `type MessageOption func(*Message)` + `WithID/WithKey/WithHeaders/WithHeader`
  - `func NewMessage(payload []byte, opts ...MessageOption) *Message`
  - 内部：`func toWatermill(m *Message) *message.Message`、`func fromWatermill(m *message.Message) *Message`（Task 4 的 Broker 使用）
  - 迁移后导出名不变：`MessageIDFromContext/ContextWithMessageID/MessageKeyFromContext/ContextWithMessageKey/MessageIDKey/MessageKeyKey`

- [ ] **Step 1: 写失败测试**

创建 `message_test.go`：

```go
package pubsub

import (
	"context"
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
)

func TestNewMessageDefaults(t *testing.T) {
	m := NewMessage([]byte("payload"))
	if m.ID == "" {
		t.Fatal("expected non-empty random ID")
	}
	if m.Key != "" {
		t.Fatalf("expected empty key, got %q", m.Key)
	}
	if m.Headers == nil {
		t.Fatal("expected non-nil headers map")
	}
	if string(m.Payload) != "payload" {
		t.Fatalf("unexpected payload: %q", m.Payload)
	}
}

func TestMessageOptions(t *testing.T) {
	m := NewMessage(nil,
		WithID("id-1"),
		WithKey("key-1"),
		WithHeader("a", "1"),
		WithHeaders(map[string]string{"b": "2"}),
	)
	if m.ID != "id-1" || m.Key != "key-1" {
		t.Fatalf("unexpected id/key: %q %q", m.ID, m.Key)
	}
	if m.Headers["a"] != "1" || m.Headers["b"] != "2" {
		t.Fatalf("unexpected headers: %+v", m.Headers)
	}
}

func TestToWatermill(t *testing.T) {
	m := NewMessage([]byte("x"), WithID("id-1"), WithKey("k1"), WithHeader("h", "v"))
	wm := toWatermill(m)
	if wm.UUID != "id-1" {
		t.Fatalf("unexpected uuid: %q", wm.UUID)
	}
	if string(wm.Payload) != "x" {
		t.Fatalf("unexpected payload: %q", wm.Payload)
	}
	if got := wm.Metadata.Get(MessageKeyKey.String()); got != "k1" {
		t.Fatalf("unexpected key in metadata: %q", got)
	}
	if got := wm.Metadata.Get("h"); got != "v" {
		t.Fatalf("unexpected header: %q", got)
	}
}

func TestFromWatermill(t *testing.T) {
	wm := message.NewMessage("id-1", []byte("x"))
	wm.Metadata.Set(MessageKeyKey.String(), "k1")
	wm.Metadata.Set("h", "v")
	m := fromWatermill(wm)
	if m.ID != "id-1" || m.Key != "k1" || string(m.Payload) != "x" {
		t.Fatalf("unexpected message: %+v", m)
	}
	if m.Headers["h"] != "v" {
		t.Fatalf("unexpected header: %+v", m.Headers)
	}
	// 协议键 x-message-key 不进入 Headers。
	if _, ok := m.Headers[MessageKeyKey.String()]; ok {
		t.Fatalf("protocol key leaked into Headers: %+v", m.Headers)
	}
}

func TestRoundTrip(t *testing.T) {
	m := NewMessage([]byte("payload"), WithKey("k1"), WithHeader("trace-id", "abc"))
	got := fromWatermill(toWatermill(m))
	if got.ID != m.ID || got.Key != m.Key || string(got.Payload) != string(m.Payload) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, m)
	}
	if got.Headers["trace-id"] != "abc" {
		t.Fatalf("round-trip header mismatch: %+v", got.Headers)
	}
}

func TestMessageContextHelpers(t *testing.T) {
	// 从旧 broker.go 迁移的 helpers 行为不变。
	if got := MessageIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty id, got %q", got)
	}
	ctx := ContextWithMessageID(context.Background(), "msg-1")
	if got := MessageIDFromContext(ctx); got != "msg-1" {
		t.Fatalf("expected msg-1, got %q", got)
	}
	ctx = ContextWithMessageKey(ctx, "key-1")
	if got := MessageKeyFromContext(ctx); got != "key-1" {
		t.Fatalf("expected key-1, got %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd contrib/pubsub && go test ./... -run "TestNewMessageDefaults|TestMessageOptions|TestToWatermill|TestFromWatermill|TestRoundTrip|TestMessageContextHelpers"`
Expected: 编译失败，`undefined: Message` / `undefined: NewMessage`

- [ ] **Step 3: 实现**

创建 `message.go`：

```go
package pubsub

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
)

// Message 是公共 API 的消息类型，与底层 Watermill 解耦。
// ID 是消息唯一标识；Key 是消息键（如 Kafka key）；Headers 承载元数据。
type Message struct {
	ID      string
	Key     string
	Headers map[string]string
	Payload []byte
}

// MessageOption 用于配置 Message 的选项函数。
type MessageOption func(*Message)

// WithID 设置消息 ID。
func WithID(id string) MessageOption {
	return func(m *Message) { m.ID = id }
}

// WithKey 设置消息 key。
func WithKey(key string) MessageOption {
	return func(m *Message) { m.Key = key }
}

// WithHeaders 合并设置消息头。
func WithHeaders(h map[string]string) MessageOption {
	return func(m *Message) {
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		for k, v := range h {
			m.Headers[k] = v
		}
	}
}

// WithHeader 添加单条消息头。
func WithHeader(k, v string) MessageOption {
	return func(m *Message) {
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		m.Headers[k] = v
	}
}

// NewMessage 创建消息，ID 缺省随机生成。
func NewMessage(payload []byte, opts ...MessageOption) *Message {
	m := &Message{ID: uuid.NewString(), Headers: map[string]string{}}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

type msgKeyCtx struct{}

func (ctx msgKeyCtx) String() string { return "x-message-key" }

// MessageKeyKey 是消息 key 的 wire 协议键（写入 watermill 元数据 / Kafka header）。
var MessageKeyKey = msgKeyCtx{}

type msgIdCtx struct{}

func (ctx msgIdCtx) String() string { return "x-message-id" }

// MessageIDKey 是消息 ID 的 wire 协议键（写入 Kafka header）。
var MessageIDKey = msgIdCtx{}

// ContextWithMessageID 将消息 ID 写入上下文。
func ContextWithMessageID(ctx context.Context, msgId string) context.Context {
	return context.WithValue(ctx, MessageIDKey, msgId)
}

// MessageIDFromContext 从上下文中获取消息 ID，未设置时返回空字符串。
func MessageIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(MessageIDKey).(string)
	return v
}

// ContextWithMessageKey 将消息 key 写入上下文。
func ContextWithMessageKey(ctx context.Context, msgKey string) context.Context {
	return context.WithValue(ctx, MessageKeyKey, msgKey)
}

// MessageKeyFromContext 从上下文中获取消息 key，未设置时返回空字符串。
func MessageKeyFromContext(ctx context.Context) string {
	v, _ := ctx.Value(MessageKeyKey).(string)
	return v
}

// toWatermill 将公共消息转换为 watermill 消息（内部使用）。
func toWatermill(m *Message) *message.Message {
	wm := message.NewMessage(m.ID, m.Payload)
	if m.Key != "" {
		wm.Metadata.Set(MessageKeyKey.String(), m.Key)
	}
	for k, v := range m.Headers {
		wm.Metadata.Set(k, v)
	}
	return wm
}

// fromWatermill 将 watermill 消息转换为公共消息（内部使用）。
// key 与协议键 x-message-key 还原为 Message.Key，不进入 Headers。
func fromWatermill(wm *message.Message) *Message {
	m := &Message{
		ID:      wm.UUID,
		Key:     wm.Metadata.Get(MessageKeyKey.String()),
		Headers: map[string]string{},
		Payload: wm.Payload,
	}
	for k, v := range wm.Metadata {
		if k == MessageKeyKey.String() {
			continue
		}
		m.Headers[k] = v
	}
	return m
}
```

修改 `broker.go`：删除以下已迁移到 message.go 的定义（保留文件其余部分不动，旧 `NewJSONMessage` 与 `Set/GetMessageKey/ID` 仍在）：

- `msgKeyCtx` 结构体与 `MessageKeyKey` 变量
- `ContextWithMessageKey` / `MessageKeyFromContext`
- `msgIdCtx` 结构体与 `MessageIDKey` 变量
- `MessageIDFromContext` / `ContextWithMessageID`

- [ ] **Step 4: 运行测试确认通过**

Run: `cd contrib/pubsub && go test ./...`
Expected: 全部通过（旧 broker 测试仍绿，因为导出名未变）

- [ ] **Step 5: 提交**

```bash
git add contrib/pubsub/message.go contrib/pubsub/message_test.go contrib/pubsub/broker.go
git commit -m "feat(pubsub): 自有 Message 类型与 watermill 互转"
```

---

### Task 3: Transport 接口与内存 Transport

**Files:**
- Create: `contrib/pubsub/transport.go`
- Create: `contrib/pubsub/memory.go`
- Test: `contrib/pubsub/memory_test.go`（新建）

**Interfaces:**
- Consumes: `lynx.ServerLike`（`health.Checker` + `lynx.Component`）、watermill `message.PubSub`、gochannel
- Produces:
  - `type SubscriptionOptions struct { Group string; Instances int }`
  - `type Transport interface { lynx.ServerLike; Publish(topic string, msgs ...*message.Message) error; Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error); Topics() []string }`
  - `func NewMemoryTransport() *MemoryTransport`（实现 Transport）

- [ ] **Step 1: 写失败测试**

创建 `memory_test.go`：

```go
package pubsub

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
)

func TestMemoryTransportPublishSubscribe(t *testing.T) {
	tp := NewMemoryTransport()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan *message.Message, 1)
	ch, err := tp.Subscribe(ctx, "test.event", SubscriptionOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go func() {
		for msg := range ch {
			received <- msg
		}
	}()

	msg := message.NewMessage("id-1", []byte("payload"))
	if err := tp.Publish("test.event", msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-received:
		if got.UUID != "id-1" || string(got.Payload) != "payload" {
			t.Fatalf("unexpected message: %+v", got)
		}
		got.Ack()
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive message within 5s")
	}
}

func TestMemoryTransportLifecycle(t *testing.T) {
	tp := NewMemoryTransport()
	if err := tp.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error before Start")
	}
	if err := tp.Init(&fakeApp{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	startCtx, startCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tp.Start(startCtx) }()

	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return tp.CheckHealth() == nil }) {
		t.Fatal("transport did not become healthy")
	}

	startCancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

var _ Transport = (*MemoryTransport)(nil)
var _ lynx.Component = (*MemoryTransport)(nil)
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd contrib/pubsub && go test ./... -run TestMemoryTransport`
Expected: 编译失败，`undefined: Transport` / `undefined: NewMemoryTransport`

- [ ] **Step 3: 实现**

创建 `transport.go`：

```go
package pubsub

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
)

// Transport 是消息代理后端：一个后端（kafka/内存/未来 redis-stream 等）
// 对应一个 Transport 组件。topic 参数一律是逻辑名，物理名解析在实现内部。
type Transport interface {
	lynx.ServerLike
	// Publish 将消息发布到逻辑 topic。
	Publish(topic string, msgs ...*message.Message) error
	// Subscribe 订阅逻辑 topic，opts 携带订阅参数（代码显式值优先于
	// Transport 自身配置）。返回的 channel 在取消时关闭。
	Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error)
	// Topics 返回该 Transport 承接的逻辑 topic 全集（Broker 自动路由用）。
	Topics() []string
}

// SubscriptionOptions 是订阅参数；Group 为空字符串、Instances 小于 1 时
// 由 Transport 按自身配置兜底。
type SubscriptionOptions struct {
	Group     string
	Instances int
}
```

创建 `memory.go`：

```go
package pubsub

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/lynx-go/lynx"
)

// MemoryTransport 是进程内 Transport，基于 watermill gochannel。
type MemoryTransport struct {
	pubSub  message.PubSub
	running atomic.Bool
}

// NewMemoryTransport 创建进程内 Transport，可作 Broker 的默认 Transport。
func NewMemoryTransport() *MemoryTransport {
	return &MemoryTransport{
		pubSub: gochannel.NewGoChannel(gochannel.Config{}, watermill.NopLogger{}),
	}
}

// Name 返回组件名称 "pubsub-memory"。
func (t *MemoryTransport) Name() string { return "pubsub-memory" }

// Init 无额外初始化工作。
func (t *MemoryTransport) Init(app lynx.App) error { return nil }

// Start 标记运行并阻塞至 ctx 取消。
func (t *MemoryTransport) Start(ctx context.Context) error {
	t.running.Store(true)
	<-ctx.Done()
	t.running.Store(false)
	return nil
}

// Stop 关闭底层 gochannel。
func (t *MemoryTransport) Stop(ctx context.Context) {
	if err := t.pubSub.Close(); err != nil {
		// gochannel.Close 在正常关闭时无错误。
	}
	t.running.Store(false)
}

// CheckHealth 报告 Transport 是否在运行。
func (t *MemoryTransport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("memory transport is not running")
	}
	return nil
}

// Publish 发布消息到逻辑 topic（gochannel 按 topic 精确匹配）。
func (t *MemoryTransport) Publish(topic string, msgs ...*message.Message) error {
	return t.pubSub.Publish(topic, msgs...)
}

// Subscribe 订阅逻辑 topic；SubscriptionOptions 对内存 Transport 无意义。
func (t *MemoryTransport) Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error) {
	return t.pubSub.Subscribe(ctx, topic)
}

// Topics 返回 nil：内存 Transport 不声明 topic，仅作默认回退。
func (t *MemoryTransport) Topics() []string { return nil }
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd contrib/pubsub && go test ./...`
Expected: 全部通过

- [ ] **Step 5: 提交**

```bash
git add contrib/pubsub/transport.go contrib/pubsub/memory.go contrib/pubsub/memory_test.go
git commit -m "feat(pubsub): Transport 接口与内存 Transport"
```

---

### Task 4: Broker 门面重写（删除 Binder）

**Files:**
- Rewrite: `contrib/pubsub/broker.go`
- Rewrite: `contrib/pubsub/router.go`
- Modify: `contrib/pubsub/message.go`（追加 `NewJSONMessage`/`MustJSONMessage`）
- Delete: `contrib/pubsub/broker_watermill.go`、`contrib/pubsub/binder.go`
- Rewrite: `contrib/pubsub/broker_test.go`

**Interfaces:**
- Consumes: Task 2 的 `Message`/`toWatermill`/`fromWatermill`、Task 3 的 `Transport`/`SubscriptionOptions`/`NewMemoryTransport`
- Produces（公共 API 定稿）:
  - `type Broker interface { lynx.ServerLike; Publish(ctx context.Context, topic string, msg *Message, opts ...PublishOption) error; Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error; Route(topic string, t Transport) }`
  - `type Options struct { Transports []Transport; DefaultTransport Transport }` + `func NewBroker(opts Options) *Broker`
  - `type HandlerFunc func(ctx context.Context, event *Message) error`
  - `type SubscribeOptions struct { AutoAck bool; ContinueOnError bool; Group string; Instances int }` + `WithAutoAck/WithContinueOnError/WithGroup/WithInstances`
  - `type PublishOptions struct { MessageKey string; Metadata map[string]string }` + `WithMessageKey/WithMetadata/WithMetadataField`
  - `func NewJSONMessage(data any, opts ...MessageOption) (*Message, error)`、`func MustJSONMessage(data any, opts ...MessageOption) *Message`
  - 临时兼容（deprecated，Task 5 删除）：`SetMessageKey/GetMessageKey/SetMessageID/GetMessageID`

> **注意**：本任务结束时 `contrib/kafka` 与 `_examples/pubsub` 预期编译失败（仍引用被删除的 Binder 旧 API）——Task 5 修复。本任务验收只看 pubsub 模块。

- [ ] **Step 1: 写失败测试（重写 broker_test.go）**

用以下内容整体替换 `broker_test.go`（`fakeApp`、`pollUntil` 从旧文件原样保留，测试语义迁移 + 新增路由/时序测试）：

```go
package pubsub

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
)

// fakeTransport 记录 Publish 调用并可注入订阅消息。
type fakeTransport struct {
	mu        sync.Mutex
	topics    []string
	published []string
	subCh     chan *message.Message
}

func newFakeTransport(topics ...string) *fakeTransport {
	return &fakeTransport{topics: topics, subCh: make(chan *message.Message)}
}

func (f *fakeTransport) Name() string             { return "fake-transport" }
func (f *fakeTransport) Init(lynx.App) error      { return nil }
func (f *fakeTransport) Start(context.Context) error { return nil }
func (f *fakeTransport) Stop(context.Context)     {}
func (f *fakeTransport) CheckHealth() error       { return nil }
func (f *fakeTransport) Topics() []string         { return f.topics }

func (f *fakeTransport) Publish(topic string, msgs ...*message.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, topic)
	return nil
}

func (f *fakeTransport) Subscribe(ctx context.Context, topic string, opts SubscriptionOptions) (<-chan *message.Message, error) {
	out := make(chan *message.Message)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-f.subCh:
				out <- msg
			}
		}
	}()
	return out, nil
}

func (f *fakeTransport) publishedTopics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.published...)
}

// startBroker 创建内存默认 Transport 的 Broker，注册订阅并启动。
func startBroker(t *testing.T, h HandlerFunc, subOpts ...SubscribeOption) (Broker, chan error) {
	t.Helper()
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(context.Background(), "test.event", "test-handler", h, subOpts...); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		cancel()
		t.Fatalf("broker did not become healthy")
	}
	// 等待 gochannel 完成订阅接线。
	time.Sleep(200 * time.Millisecond)

	t.Cleanup(func() {
		cancel()
		b.Stop(context.Background())
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("broker did not stop within 3s")
		}
	})
	return b, done
}

func TestBrokerBeforeInit(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error before Init")
	}
}

func TestBrokerPublishSubscribe(t *testing.T) {
	type result struct {
		ctx context.Context
		msg *Message
	}
	received := make(chan result, 1)

	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		received <- result{ctx: ctx, msg: msg}
		return nil
	})

	published := MustJSONMessage(map[string]string{"hello": "world"})
	if err := b.Publish(context.Background(), "test.event", published,
		WithMessageKey("key-1"),
		WithMetadataField("foo", "bar"),
	); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case r := <-received:
		if string(r.msg.Payload) != string(published.Payload) {
			t.Errorf("payload = %s, want %s", r.msg.Payload, published.Payload)
		}
		if r.msg.Key != "key-1" {
			t.Errorf("key = %q, want key-1", r.msg.Key)
		}
		if r.msg.Headers["foo"] != "bar" {
			t.Errorf("header foo = %q, want bar", r.msg.Headers["foo"])
		}
		if got := MessageIDFromContext(r.ctx); got != published.ID {
			t.Errorf("MessageIDFromContext() = %q, want %q", got, published.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive published message within 5s")
	}
}

func TestBrokerStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(ctx, "noop.event", "noop-handler", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return b.CheckHealth() == nil }) {
		t.Fatal("broker did not start")
	}
	cancel()
	b.Stop(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

func TestBrokerRetriesFailedHandler(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		if calls.Add(1) >= 2 {
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return errors.New("handler failed")
	})

	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler was not retried (calls=%d)", calls.Load())
	}
}

func TestBrokerContinueOnErrorAcks(t *testing.T) {
	var calls atomic.Int32
	receivedSecond := make(chan struct{}, 1)
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error {
		if calls.Add(1) == 1 {
			return errors.New("first message fails")
		}
		select {
		case receivedSecond <- struct{}{}:
		default:
		}
		return nil
	}, WithContinueOnError())

	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish first: %v", err)
	}
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("first message was not processed")
	}
	if err := b.CheckHealth(); err != nil {
		t.Fatalf("broker unhealthy after failing handler: %v", err)
	}
	if err := b.Publish(context.Background(), "test.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish second: %v", err)
	}
	select {
	case <-receivedSecond:
	case <-time.After(5 * time.Second):
		t.Fatal("second message was not delivered after ContinueOnError")
	}
}

func TestBrokerRouteExplicitAndFallback(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{DefaultTransport: NewMemoryTransport()})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	b.Route("orders", ft)

	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish routed: %v", err)
	}
	if got := ft.publishedTopics(); len(got) != 1 || got[0] != "orders" {
		t.Fatalf("expected routed publish, got %v", got)
	}

	// 未命中路由表的 topic 走默认 Transport（内存）——不报错。
	if err := b.Publish(context.Background(), "local.event", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish fallback: %v", err)
	}
}

func TestBrokerPublishNoTransport(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err == nil {
		t.Fatal("expected Publish error with no route and no default transport")
	}
}

func TestBrokerSubscribeNoTransportFailsAtStart(t *testing.T) {
	b := NewBroker(Options{})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe(context.Background(), "orders", "h", func(ctx context.Context, msg *Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe buffered: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("expected Start error for un-routed subscription")
	}
}

func TestBrokerAutoRouteFromTransports(t *testing.T) {
	ft := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft}})
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", MustJSONMessage(nil)); err != nil {
		t.Fatalf("Publish auto-routed: %v", err)
	}
	if got := ft.publishedTopics(); len(got) != 1 {
		t.Fatalf("expected auto-routed publish, got %v", got)
	}
}

func TestBrokerAutoRouteConflict(t *testing.T) {
	ft1 := newFakeTransport("orders")
	ft2 := newFakeTransport("orders")
	b := NewBroker(Options{Transports: []Transport{ft1, ft2}})
	if err := b.Init(newFakeApp()); err == nil {
		t.Fatal("expected Init error for conflicting auto routes")
	}
}

func TestBrokerSubscribeAfterStartFails(t *testing.T) {
	b, _ := startBroker(t, func(ctx context.Context, msg *Message) error { return nil })
	if err := b.Subscribe(context.Background(), "late.event", "late-handler", func(ctx context.Context, msg *Message) error {
		return nil
	}); err == nil {
		t.Fatal("expected Subscribe error after Start")
	}
}
```

> 新 `broker_test.go` 的 import 块（完整）：

```go
import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
)
```

> 旧 broker_test.go 的 `fakeApp` 与 `pollUntil` 原样保留在新文件中（fakeApp 已实现 `lynx.App`，`pollUntil` 为轮询辅助）。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd contrib/pubsub && go test ./...`
Expected: 编译失败（旧 Broker 接口与 `NewBroker(opts Options)` 均未定义；`Start` 返回值等不匹配）

- [ ] **Step 3: 实现 broker.go 重写**

用以下内容整体替换 `contrib/pubsub/broker.go`：

```go
// Package pubsub 提供基于 Watermill 的消息发布订阅抽象：
// Broker 门面、Transport 后端与消息 Handler。
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

// Broker 是消息代理门面组件：按 topic 路由到 Transport，统一发布订阅。
type Broker interface {
	lynx.ServerLike
	// Publish 将消息发布到逻辑 topic；路由表未命中时走默认 Transport。
	Publish(ctx context.Context, topic string, msg *Message, opts ...PublishOption) error
	// Subscribe 注册 topic 的消费 handler。Start 前调用为缓冲注册，
	// Start 后调用返回错误。
	Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error
	// Route 显式将 topic 路由到指定 Transport，覆盖自动路由。
	Route(topic string, t Transport)
}

// Options 是 Broker 的配置项。
type Options struct {
	// Transports 参与自动路由：每个 Transport.Topics() 声明的 topic
	// 自动路由到该 Transport；重复声明同一 topic 时 Init 报错。
	Transports []Transport
	// DefaultTransport 承接路由表未命中的 topic。
	DefaultTransport Transport
}

// NewBroker 创建消息代理门面。
func NewBroker(opts Options) *Broker {
	return &Broker{options: opts, routes: map[string]Transport{}}
}

// HandlerFunc 是事件处理函数，返回错误时按订阅选项决定重试或确认。
type HandlerFunc func(ctx context.Context, event *Message) error

// Handler 定义事件处理器的元信息与处理函数。
type Handler interface {
	EventName() string
	HandlerName() string
	HandlerFunc() HandlerFunc
}

// HandlerOptions 可为 Handler 附加订阅选项。
type HandlerOptions interface {
	Options() []SubscribeOption
}

// SubscribeOptions 是订阅行为的配置项。
type SubscribeOptions struct {
	AutoAck         bool   `json:"auto_ack"`
	ContinueOnError bool   `json:"continue_on_error"`
	Group           string `json:"group"`
	Instances       int    `json:"instances"`
}

// SubscribeOption 用于配置 SubscribeOptions 的选项函数。
type SubscribeOption func(*SubscribeOptions)

// WithAutoAck 设置订阅为自动确认：消息到达即 Ack，处理失败不影响确认。
func WithAutoAck() SubscribeOption {
	return func(opts *SubscribeOptions) { opts.AutoAck = true }
}

// WithContinueOnError 设置处理失败时仍确认消息，不再重投。
func WithContinueOnError() SubscribeOption {
	return func(opts *SubscribeOptions) { opts.ContinueOnError = true }
}

// WithGroup 显式指定消费组，覆盖 Transport 配置的默认组。
func WithGroup(group string) SubscribeOption {
	return func(opts *SubscribeOptions) { opts.Group = group }
}

// WithInstances 显式指定同组消费者成员数，覆盖 Transport 配置的默认值。
func WithInstances(n int) SubscribeOption {
	return func(opts *SubscribeOptions) { opts.Instances = n }
}

// PublishOptions 是发布行为的配置项。
type PublishOptions struct {
	MessageKey string            `json:"message_key"`
	Metadata   map[string]string `json:"metadata"`
}

// PublishOption 用于配置 PublishOptions 的选项函数。
type PublishOption func(*PublishOptions)

// WithMessageKey 设置消息 key，发布时写入消息 Key 字段。
func WithMessageKey(key string) PublishOption {
	return func(opts *PublishOptions) { opts.MessageKey = key }
}

// WithMetadata 设置消息元数据，发布时合并进消息头。
func WithMetadata(metadata map[string]string) PublishOption {
	return func(opts *PublishOptions) { opts.Metadata = metadata }
}

// WithMetadataField 添加单条消息元数据字段。
func WithMetadataField(key, value string) PublishOption {
	return func(opts *PublishOptions) {
		if opts.Metadata == nil {
			opts.Metadata = map[string]string{}
		}
		opts.Metadata[key] = value
	}
}

type pendingSubscription struct {
	topic       string
	handlerName string
	handler     HandlerFunc
	opts        SubscribeOptions
}

// subscriberAdapter 将 Transport 适配为 watermill 的 Subscriber。
type subscriberAdapter struct {
	t    Transport
	opts SubscriptionOptions
}

func (a subscriberAdapter) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	return a.t.Subscribe(ctx, topic, a.opts)
}

// Broker 是 Broker 接口的具体实现。
type Broker struct {
	options Options
	app     lynx.App
	router  *message.Router
	routes  map[string]Transport
	mu      sync.Mutex
	pending []pendingSubscription
	started bool
}

// Name 返回组件名称 "pubsub-broker"。
func (b *Broker) Name() string { return "pubsub-broker" }

// Route 显式将 topic 路由到指定 Transport，覆盖自动路由。
func (b *Broker) Route(topic string, t Transport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.routes[topic] = t
}

// CheckHealth 报告 Broker 是否在运行。
func (b *Broker) CheckHealth() error {
	if b.router == nil {
		return errors.New("broker is not initialized")
	}
	if b.router.IsRunning() {
		return nil
	}
	return errors.New("broker is not running")
}

// Init 创建 watermill router 并执行自动路由。
func (b *Broker) Init(app lynx.App) error {
	b.app = app
	slogger := app.Logger("component", "pubsub")
	logger := watermill.NewSlogLogger(slogger)

	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return err
	}
	router.AddMiddleware(
		middleware.Recoverer,
		middleware.CorrelationID,
		middleware.Retry{MaxRetries: 3}.Middleware,
	)
	router.AddPlugin(plugin.SignalsHandler)
	b.router = router

	for _, t := range b.options.Transports {
		for _, topic := range t.Topics() {
			if prev, ok := b.routes[topic]; ok && prev != t {
				return fmt.Errorf("topic %q is routed to multiple transports", topic)
			}
			b.routes[topic] = t
		}
	}
	return nil
}

func (b *Broker) resolve(topic string) (Transport, error) {
	if t, ok := b.routes[topic]; ok {
		return t, nil
	}
	if b.options.DefaultTransport != nil {
		return b.options.DefaultTransport, nil
	}
	return nil, fmt.Errorf("no transport routed for topic %q", topic)
}

// Start 将缓冲订阅统一注册进 watermill router 并运行；任一订阅
// 无归属 Transport 时返回错误。
func (b *Broker) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return errors.New("broker already started")
	}
	b.started = true
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()

	for _, p := range pending {
		t, err := b.resolve(p.topic)
		if err != nil {
			return err
		}
		adapter := subscriberAdapter{
			t:    t,
			opts: SubscriptionOptions{Group: p.opts.Group, Instances: p.opts.Instances},
		}
		b.router.AddConsumerHandler(p.handlerName, p.topic, adapter, b.wrapHandler(p.handler, p.opts))
	}
	return b.router.Run(ctx)
}

// Stop 关闭 watermill router。
func (b *Broker) Stop(ctx context.Context) {
	if b.router != nil {
		if err := b.router.Close(); err != nil {
			log.ErrorContext(ctx, "error closing router", err)
		}
	}
}

// Subscribe 缓冲注册订阅；Start 后调用返回错误。
func (b *Broker) Subscribe(ctx context.Context, topic, handlerName string, h HandlerFunc, opts ...SubscribeOption) error {
	o := &SubscribeOptions{}
	for _, opt := range opts {
		opt(o)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return errors.New("cannot subscribe to a started broker")
	}
	b.pending = append(b.pending, pendingSubscription{
		topic: topic, handlerName: handlerName, handler: h, opts: *o,
	})
	return nil
}

// wrapHandler 包装用户 handler：注入消息 ID/key 上下文，统一 Ack 语义。
func (b *Broker) wrapHandler(h HandlerFunc, o SubscribeOptions) message.NoPublishHandlerFunc {
	handler := func(msg *message.Message) error {
		ctx := ContextWithMessageID(msg.Context(), msg.UUID)
		ctx = ContextWithMessageKey(ctx, msg.Metadata.Get(MessageKeyKey.String()))
		ctx = log.Context(ctx, log.FromContext(ctx), MessageIDKey.String(), msg.UUID)

		if err := h(ctx, fromWatermill(msg)); err != nil {
			log.ErrorContext(ctx, "error handling message", err, "x-message-id", msg.UUID)
			if o.ContinueOnError {
				msg.Ack()
				return nil
			}
			return err
		}
		msg.Ack()
		return nil
	}
	if o.AutoAck {
		return func(msg *message.Message) error {
			msg.Ack()
			return handler(msg)
		}
	}
	return handler
}

// Publish 将消息发布到逻辑 topic；路由未命中且无默认 Transport 时返回错误。
func (b *Broker) Publish(ctx context.Context, topic string, msg *Message, opts ...PublishOption) error {
	o := &PublishOptions{}
	for _, opt := range opts {
		opt(o)
	}
	if o.MessageKey != "" {
		msg.Key = o.MessageKey
	}
	for k, v := range o.Metadata {
		if msg.Headers == nil {
			msg.Headers = map[string]string{}
		}
		msg.Headers[k] = v
	}
	t, err := b.resolve(topic)
	if err != nil {
		return err
	}
	return t.Publish(topic, toWatermill(msg))
}

// SetMessageKey 将消息 key 写入 watermill 消息元数据。
//
// Deprecated: 使用 Message 字段与 WithKey。
func SetMessageKey(msg *message.Message, key string) {
	msg.Metadata.Set(MessageKeyKey.String(), key)
}

// GetMessageKey 从 watermill 消息元数据中读取消息 key。
//
// Deprecated: 使用 Message 字段。
func GetMessageKey(msg *message.Message) string {
	return msg.Metadata.Get(MessageKeyKey.String())
}

// SetMessageID 将消息 ID 写入 watermill 消息元数据。
//
// Deprecated: 使用 Message 字段与 WithID。
func SetMessageID(msg *message.Message, msgId string) {
	msg.Metadata.Set(MessageIDKey.String(), msgId)
}

// GetMessageID 从 watermill 消息元数据中读取消息 ID。
//
// Deprecated: 使用 Message 字段。
func GetMessageID(msg *message.Message) string {
	return msg.Metadata.Get(MessageIDKey.String())
}
```

在 `message.go` 末尾追加 JSON 构造器（import 增加标准库 `"encoding/json"`）：

```go
// NewJSONMessage 将数据 JSON 序列化后创建消息。
func NewJSONMessage(data any, opts ...MessageOption) (*Message, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return NewMessage(bytes, opts...), nil
}

// MustJSONMessage 将数据 JSON 序列化后创建消息，序列化失败时 panic。
func MustJSONMessage(data any, opts ...MessageOption) *Message {
	msg, err := NewJSONMessage(data, opts...)
	if err != nil {
		panic(err)
	}
	return msg
}
```

删除文件：`contrib/pubsub/broker_watermill.go`、`contrib/pubsub/binder.go`。

- [ ] **Step 4: 重写 router.go**

用以下内容整体替换 `contrib/pubsub/router.go`（旧实现引用已被删除的 `Binders()`/`CanSubscribe`，必须同步重写）：

```go
package pubsub

import (
	"context"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

// Router 是事件路由组件：Init 期把全部 Handler 缓冲订阅到 Broker。
type Router struct {
	broker   Broker
	handlers []Handler
	ctx      context.Context
	cancel   context.CancelFunc
}

// Name 返回组件名称 "pubsub-router"。
func (r *Router) Name() string { return "pubsub-router" }

// Init 将全部 Handler 缓冲订阅到 Broker（纯缓冲，无时序依赖）。
func (r *Router) Init(app lynx.App) error {
	for _, h := range r.handlers {
		log.InfoContext(app.Context(), "add event handler", "event_name", h.EventName(), "handler_name", h.HandlerName())
		var opts []SubscribeOption
		if o, ok := h.(HandlerOptions); ok {
			opts = append(opts, o.Options()...)
		}
		if err := r.broker.Subscribe(app.Context(), h.EventName(), h.HandlerName(), h.HandlerFunc(), opts...); err != nil {
			return err
		}
	}
	return nil
}

// Start 阻塞至组件停止。
func (r *Router) Start(ctx context.Context) error {
	<-r.ctx.Done()
	return nil
}

// Stop 取消路由上下文，使 Start 返回。
func (r *Router) Stop(ctx context.Context) {
	r.cancel()
}

var _ lynx.Component = (*Router)(nil)

// NewRouter 创建事件路由组件。
func NewRouter(broker Broker, handlers []Handler) *Router {
	ctx, cancel := context.WithCancel(context.Background())
	return &Router{broker: broker, handlers: handlers, ctx: ctx, cancel: cancel}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd contrib/pubsub && go mod tidy && go test ./...`
Expected: 全部通过。若 `go mod tidy` 提示移除 `go.uber.org/multierr` 等不再使用的依赖，接受该变更（正常）。

> **预期过渡态**：`cd contrib/kafka && go build ./...` 与 `cd _examples && go build ./...` 现在失败（旧 Binder API 已删除）——Task 5 修复，无需处理。

- [ ] **Step 6: 提交**

```bash
git add contrib/pubsub/
git commit -m "refactor(pubsub): Broker 门面 + 路由表，删除 Binder"
```

---

### Task 5: Kafka Transport 重写 + 删除旧 kafka 代码 + 示例重写

**Files:**
- Create: `contrib/kafka/transport.go`
- Create: `contrib/kafka/transport_test.go`
- Delete: `contrib/kafka/binder.go`、`consumer.go`、`producer.go`、`binder_test.go`、`consumer_test.go`、`producer_test.go`、`fakes_test.go`
- Rewrite: `_examples/pubsub/main.go`
- Create: `_examples/pubsub/config.yaml`
- Modify: `contrib/kafka/go.mod`、`contrib/pubsub/go.mod`

**Interfaces:**
- Consumes: Task 1 的 `UnmarshalKey`、Task 3 的 `pubsub.Transport`/`SubscriptionOptions`
- Produces:
  - `type Options struct { Topics map[string]TopicOptions }`（`mapstructure:",remain"`）
  - `type TopicOptions struct { Brokers []string; Topics []string; Consumer *ConsumerOptions; Producer *ProducerOptions }`
  - `type ConsumerOptions struct { GroupID string; Instances int; CommitInterval time.Duration; LogMessage bool }`
  - `type ProducerOptions struct { Topic string; LogMessage bool; BatchSize int }`
  - `func NewTransport(opts Options) (*Transport, error)`
  - `(*Transport)` 实现 `pubsub.Transport` + `lynx.Component`
  - 依赖新增：`github.com/ThreeDotsLabs/watermill-kafka/v3 v3.1.0`、`github.com/IBM/sarama`

- [ ] **Step 1: 写失败测试**

创建 `transport_test.go`：

```go
package kafka

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/spf13/viper"
)

// fakePubSub 是 client seam 的 fake：记录 Publish/Subscribe 调用。
type fakePubSub struct {
	mu        sync.Mutex
	published []string
	subChs    map[string][]chan *message.Message // key: "group|topic"
	closed    atomic.Int32
}

func newFakePubSub() *fakePubSub {
	return &fakePubSub{subChs: map[string][]chan *message.Message{}}
}

func (f *fakePubSub) Publish(topic string, msgs ...*message.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, topic)
	return nil
}

func (f *fakePubSub) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	ch := make(chan *message.Message)
	f.mu.Lock()
	f.subChs[topic] = append(f.subChs[topic], ch)
	f.mu.Unlock()
	return ch, nil
}

func (f *fakePubSub) Close() error {
	f.closed.Add(1)
	return nil
}

func (f *fakePubSub) publishTopics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.published...)
}

func (f *fakePubSub) subscribeCount(topic string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subChs[topic])
}

// newTestTransport 构造注入 fake client seam 的 Transport。
func newTestTransport(opts Options, pub *fakePubSub) *Transport {
	t := &Transport{
		opts:          opts,
		publishers:    map[string]message.Publisher{},
		subscribers:   map[string]message.Subscriber{},
		saramaConfigs: map[string]*sarama.Config{},
		newPublisher: func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error) {
			return pub, nil
		},
		newSubscriber: func(brokers []string, group string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Subscriber, error) {
			return pub, nil
		},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t
}

func TestOptionsFromConfig(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
kafka:
  orders:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_orders, topic_orders_v2]
    consumer:
      group_id: orders-group
      instances: 3
      commit_interval: 1s
      log_message: true
    producer:
      topic: topic_orders_v2
      log_message: true
      batch_size: 100
  payments:
    brokers: ["10.0.0.2:9092"]
    topics: [payments_topic]
    consumer:
      group_id: payments-group
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	var opts Options
	if err := v.UnmarshalKey("kafka", &opts); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	if len(opts.Topics) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(opts.Topics))
	}
	orders, ok := opts.Topics["orders"]
	if !ok {
		t.Fatalf("missing orders: %+v", opts.Topics)
	}
	if len(orders.Brokers) != 1 || orders.Brokers[0] != "127.0.0.1:19092" {
		t.Fatalf("bad brokers: %+v", orders.Brokers)
	}
	if len(orders.Topics) != 2 || orders.Topics[1] != "topic_orders_v2" {
		t.Fatalf("bad topics: %+v", orders.Topics)
	}
	if orders.Consumer == nil || orders.Consumer.GroupID != "orders-group" || orders.Consumer.Instances != 3 {
		t.Fatalf("bad consumer: %+v", orders.Consumer)
	}
	if orders.Consumer.CommitInterval != time.Second {
		t.Fatalf("bad commit interval: %v", orders.Consumer.CommitInterval)
	}
	if orders.Producer == nil || orders.Producer.Topic != "topic_orders_v2" || !orders.Producer.LogMessage {
		t.Fatalf("bad producer: %+v", orders.Producer)
	}
	if orders.Producer.BatchSize != 100 {
		t.Fatalf("bad batch size: %d", orders.Producer.BatchSize)
	}
}

func TestTransportTopics(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders":   {Brokers: []string{"b"}, Topics: []string{"t1"}},
			"payments": {Brokers: []string{"b"}, Topics: []string{"t2"}},
		},
	}, pub)
	got := tr.Topics()
	if len(got) != 2 {
		t.Fatalf("expected 2 topics, got %v", got)
	}
}

func TestTransportPublishResolvesPhysicalTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers: []string{"b1"},
				Topics:  []string{"t1", "t2"},
				Producer: &ProducerOptions{Topic: "t2"},
			},
		},
	}, pub)

	if err := tr.Publish("orders", message.NewMessage("id", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := pub.publishTopics(); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("expected publish to t2, got %v", got)
	}
}

func TestTransportPublishDefaultPhysicalTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b1"}, Topics: []string{"t1", "t2"}},
		},
	}, pub)

	if err := tr.Publish("orders", message.NewMessage("id", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := pub.publishTopics(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("expected publish to t1 (Topics[0]), got %v", got)
	}
}

func TestTransportPublishNoProducerConfig(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}, Consumer: &ConsumerOptions{GroupID: "g"}},
		},
	}, pub)

	if err := tr.Publish("orders", message.NewMessage("id", nil)); err == nil {
		t.Fatal("expected Publish error without producer config")
	}
}

func TestTransportPublishUnknownTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{}}, pub)
	if err := tr.Publish("nope", message.NewMessage("id", nil)); err == nil {
		t.Fatal("expected Publish error for unknown topic")
	}
}

func TestTransportSubscribeExpansion(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1", "t2"},
				Consumer: &ConsumerOptions{GroupID: "g1", Instances: 3},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", pubsub.SubscriptionOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// 2 物理 topic × 3 实例 = 6 个底层订阅。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pub.subscribeCount("t1") == 3 && pub.subscribeCount("t2") == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pub.subscribeCount("t1") != 3 || pub.subscribeCount("t2") != 3 {
		t.Fatalf("expected 3 subscriptions per topic, got t1=%d t2=%d",
			pub.subscribeCount("t1"), pub.subscribeCount("t2"))
	}

	// fan-in：来自两个物理 topic 的消息都能收到。
	sent := message.NewMessage("id-1", []byte("x"))
	ch1 := pub.subChs["t1"][0]
	go func() { ch1 <- sent }()
	select {
	case got := <-ch:
		if got.UUID != "id-1" {
			t.Fatalf("unexpected message: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive fan-in message")
	}
}

func TestTransportSubscribeGroupOverride(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Consumer: &ConsumerOptions{GroupID: "config-group"},
			},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tr.Subscribe(ctx, "orders", pubsub.SubscriptionOptions{Group: "code-group"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	// fake 不区分组（Subscribe 只按 topic 记录），此处仅验证不报错。
}

func TestTransportSubscribeMissingGroup(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b1"}, Topics: []string{"t1"}},
		},
	}, pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "orders", pubsub.SubscriptionOptions{}); err == nil {
		t.Fatal("expected Subscribe error when group is missing")
	}
}

func TestTransportSubscribeUnknownTopic(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{Topics: map[string]TopicOptions{}}, pub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tr.Subscribe(ctx, "nope", pubsub.SubscriptionOptions{}); err == nil {
		t.Fatal("expected Subscribe error for unknown topic")
	}
}

func TestTransportInitValidation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"missing brokers", Options{Topics: map[string]TopicOptions{
			"a": {Topics: []string{"t"}},
		}}},
		{"missing topics", Options{Topics: map[string]TopicOptions{
			"a": {Brokers: []string{"b"}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := newFakePubSub()
			tr := newTestTransport(tt.opts, pub)
			if err := tr.Init(newFakeApp()); err == nil {
				t.Fatal("expected Init error")
			}
		})
	}
}

func TestTransportLifecycle(t *testing.T) {
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {Brokers: []string{"b"}, Topics: []string{"t"}, Consumer: &ConsumerOptions{GroupID: "g"}},
		},
	}, pub)
	if err := tr.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := tr.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error before Start")
	}

	startCtx, startCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Start(startCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tr.CheckHealth() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tr.CheckHealth(); err != nil {
		t.Fatalf("transport unhealthy after Start: %v", err)
	}

	tr.Stop(context.Background())
	startCancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	if pub.closed.Load() < 1 {
		t.Fatal("expected client Close on Stop")
	}
	if err := tr.CheckHealth(); err == nil {
		t.Fatal("expected CheckHealth error after Stop")
	}
}

func TestTransportEndToEndWithRouter(t *testing.T) {
	// 集成形态：Broker + fake kafka Transport + 内存默认 Transport，
	// 验证从 Publish 到 handler 的全链路（fake 注入消息）。
	pub := newFakePubSub()
	tr := newTestTransport(Options{
		Topics: map[string]TopicOptions{
			"orders": {
				Brokers:  []string{"b1"},
				Topics:   []string{"t1"},
				Consumer: &ConsumerOptions{GroupID: "g1"},
			},
		},
	}, pub)

	broker := pubsub.NewBroker(pubsub.Options{Transports: []pubsub.Transport{tr}})
	app := newFakeApp()
	if err := broker.Init(app); err != nil {
		t.Fatalf("broker Init: %v", err)
	}

	received := make(chan *pubsub.Message, 1)
	if err := broker.Subscribe(context.Background(), "orders", "h1",
		func(ctx context.Context, msg *pubsub.Message) error {
			received <- msg
			return nil
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- broker.Start(ctx) }()

	// 从 fake kafka 客户端注入一条消息：走 Transport 订阅 → broker → handler。
	// 等待订阅建立后注入。
	deadline := time.Now().Add(5 * time.Second)
	var subCh chan *message.Message
	for time.Now().Before(deadline) {
		if pub.subscribeCount("t1") == 1 {
			subCh = pub.subChs["t1"][0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if subCh == nil {
		cancel()
		t.Fatal("subscription was not established")
	}
	subCh <- message.NewMessage("id-1", []byte("payload"))

	select {
	case got := <-received:
		if got.ID != "id-1" || string(got.Payload) != "payload" {
			t.Fatalf("unexpected message: %+v", got)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("handler did not receive message")
	}
}

// --- fakeApp：最小 lynx.App（局部定义，避免依赖旧 fakes_test.go） ---

type fakeApp struct{}

func (a *fakeApp) Close()                                    {}
func (a *fakeApp) Config() lynx.Config                      { return lynx.NewViperConfig(viper.New()) }
func (a *fakeApp) Context() context.Context                  { return context.Background() }
func (a *fakeApp) CLI(lynx.CommandFunc) error                { return nil }
func (a *fakeApp) OnStart(...lynx.HookFunc)                  {}
func (a *fakeApp) OnStop(...lynx.HookFunc)                   {}
func (a *fakeApp) Register(...lynx.Component)                {}
func (a *fakeApp) RegisterBuilders(...lynx.ComponentBuilder) {}
func (a *fakeApp) HealthCheckFunc() lynx.HealthCheckFunc     { return nil }
func (a *fakeApp) Run() error                                { return nil }
func (a *fakeApp) SetLogger(_ *slog.Logger)                  {}
func (a *fakeApp) Logger(_ ...any) *slog.Logger              { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
```

> import 要求：`io`、`log/slog` 仅供 fakeApp 使用；`github.com/IBM/sarama` 供 `newTestTransport` 的 factory 签名使用；其余见上方 import 块。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd contrib/kafka && go mod tidy && go test ./... -run "TestOptionsFromConfig|TestTransport"`
Expected: 编译失败，`undefined: Options` / `undefined: NewTransport`

- [ ] **Step 3: 实现 transport.go**

创建 `transport.go`：

```go
// Package kafka 提供 Kafka Transport 组件：按逻辑 topic 配置集群、
// 物理主题与消费/发布参数，接入 pubsub.Broker。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/x/log"
)

// Options 是 Kafka Transport 的配置；可用 app.Config().UnmarshalKey("kafka", &opts)
// 从配置文件整表加载，结构为 map[逻辑topic]TopicOptions。
type Options struct {
	Topics map[string]TopicOptions `mapstructure:",remain"`
}

// TopicOptions 是一个逻辑 topic 的完整配置。
type TopicOptions struct {
	Brokers  []string          `mapstructure:"brokers"`  // Kafka 集群地址，必填
	Topics   []string          `mapstructure:"topics"`   // 订阅的物理 topic 列表
	Consumer *ConsumerOptions  `mapstructure:"consumer"` // nil = 该 topic 只发布
	Producer *ProducerOptions  `mapstructure:"producer"` // nil = 该 topic 只订阅
}

// ConsumerOptions 是消费侧配置。
type ConsumerOptions struct {
	GroupID        string        `mapstructure:"group_id"`
	Instances      int           `mapstructure:"instances"`
	CommitInterval time.Duration `mapstructure:"commit_interval"`
	LogMessage     bool          `mapstructure:"log_message"`
}

// ProducerOptions 是发布侧配置。
type ProducerOptions struct {
	Topic      string `mapstructure:"topic"` // 发布物理 topic，缺省 = Topics[0]
	LogMessage bool   `mapstructure:"log_message"`
	BatchSize  int    `mapstructure:"batch_size"`
}

// Transport 是 Kafka 后端组件：内部按 brokers 分组客户端，
// 订阅按（消费组 × 物理 topic × 实例数）展开后 fan-in。
type Transport struct {
	opts Options
	app  lynx.App

	mu            sync.Mutex
	publishers    map[string]message.Publisher  // key: brokers 列表
	subscribers   map[string]message.Subscriber // key: "brokers|group"
	saramaConfigs map[string]*sarama.Config     // key: brokers 列表（同集群共享客户端配置）

	// 客户端工厂 seam：测试注入 fake。
	newPublisher  func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error)
	newSubscriber func(brokers []string, group string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Subscriber, error)

	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewTransport 创建 Kafka Transport。
func NewTransport(opts Options) (*Transport, error) {
	t := &Transport{
		opts:          opts,
		publishers:    map[string]message.Publisher{},
		subscribers:   map[string]message.Subscriber{},
		saramaConfigs: map[string]*sarama.Config{},
		newPublisher: func(brokers []string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Publisher, error) {
			return watermillkafka.NewPublisher(watermillkafka.PublisherConfig{Brokers: brokers, OverwriteSaramaConfig: cfg}, logger)
		},
		newSubscriber: func(brokers []string, group string, cfg *sarama.Config, logger watermill.LoggerAdapter) (message.Subscriber, error) {
			subCfg := watermillkafka.SubscriberConfig{Brokers: brokers, ConsumerGroup: group, OverwriteSaramaConfig: cfg}
			return watermillkafka.NewSubscriber(subCfg, logger)
		},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t, nil
}

// Name 返回组件名称 "kafka-transport"。
func (t *Transport) Name() string { return "kafka-transport" }

// Init 校验配置并保存应用实例。
func (t *Transport) Init(app lynx.App) error {
	t.app = app
	for name, topic := range t.opts.Topics {
		if len(topic.Brokers) == 0 {
			return fmt.Errorf("kafka: topic %q has no brokers", name)
		}
		if len(topic.Topics) == 0 {
			return fmt.Errorf("kafka: topic %q has no physical topics", name)
		}
	}
	return nil
}

// Start 标记运行并阻塞至组件停止。
func (t *Transport) Start(ctx context.Context) error {
	t.running.Store(true)
	<-t.ctx.Done()
	t.running.Store(false)
	return nil
}

// Stop 关闭全部客户端并取消组件上下文。
func (t *Transport) Stop(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.publishers {
		if err := p.Close(); err != nil {
			log.ErrorContext(ctx, "error closing kafka publisher", err)
		}
	}
	for _, s := range t.subscribers {
		if err := s.Close(); err != nil {
			log.ErrorContext(ctx, "error closing kafka subscriber", err)
		}
	}
	t.running.Store(false)
	t.cancel()
}

// CheckHealth 报告 Transport 是否在运行。
func (t *Transport) CheckHealth() error {
	if !t.running.Load() {
		return errors.New("kafka transport is not running")
	}
	return nil
}

// Topics 返回配置的逻辑 topic 全集（Broker 自动路由用）。
func (t *Transport) Topics() []string {
	names := make([]string, 0, len(t.opts.Topics))
	for name := range t.opts.Topics {
		names = append(names, name)
	}
	return names
}

func (t *Transport) logger() watermill.LoggerAdapter {
	if t.app == nil {
		return watermill.NopLogger{}
	}
	return watermill.NewSlogLogger(t.app.Logger("component", "kafka"))
}

// Publish 将消息发布到逻辑 topic 对应的物理 topic。
func (t *Transport) Publish(topic string, msgs ...*message.Message) error {
	to, ok := t.opts.Topics[topic]
	if !ok {
		return fmt.Errorf("kafka: topic %q not configured", topic)
	}
	if to.Producer == nil {
		return fmt.Errorf("kafka: topic %q has no producer config", topic)
	}
	physical := to.Producer.Topic
	if physical == "" {
		if len(to.Topics) == 0 {
			return fmt.Errorf("kafka: topic %q has no physical topics", topic)
		}
		physical = to.Topics[0]
	}
	batchSize := 0
	if to.Producer.BatchSize > 0 {
		batchSize = to.Producer.BatchSize
	}
	p, err := t.publisherFor(to.Brokers, batchSize)
	if err != nil {
		return err
	}
	if to.Producer.LogMessage {
		for _, msg := range msgs {
			log.DebugContext(t.app.Context(), "sending kafka message", "message", string(msg.Payload), "topic", physical)
		}
	}
	return p.Publish(physical, msgs...)
}

// Subscribe 订阅逻辑 topic：按（消费组 × 物理 topic × 实例数）展开，
// 全部消息 fan-in 到单一返回 channel。
func (t *Transport) Subscribe(ctx context.Context, topic string, opts pubsub.SubscriptionOptions) (<-chan *message.Message, error) {
	to, ok := t.opts.Topics[topic]
	if !ok {
		return nil, fmt.Errorf("kafka: topic %q not configured", topic)
	}
	if to.Consumer == nil {
		return nil, fmt.Errorf("kafka: topic %q has no consumer config", topic)
	}
	group := opts.Group
	if group == "" {
		group = to.Consumer.GroupID
	}
	if group == "" {
		return nil, fmt.Errorf("kafka: consumer group required for topic %q (config consumer.group_id or WithGroup)", topic)
	}
	instances := opts.Instances
	if instances < 1 {
		instances = to.Consumer.Instances
	}
	if instances < 1 {
		instances = 1
	}

	commitInterval := time.Duration(0)
	if to.Consumer.CommitInterval > 0 {
		commitInterval = to.Consumer.CommitInterval
	}
	sub, err := t.subscriberFor(to.Brokers, group, commitInterval)
	if err != nil {
		return nil, err
	}

	chans := make([]<-chan *message.Message, 0, len(to.Topics)*instances)
	for _, physical := range to.Topics {
		for i := 0; i < instances; i++ {
			ch, err := sub.Subscribe(ctx, physical)
			if err != nil {
				return nil, err
			}
			chans = append(chans, ch)
		}
	}
	return fanIn(chans), nil
}

// buildSaramaConfig 按集群构建 sarama.Config：首个 topic 的便捷参数
// （CommitInterval / BatchSize）生效，同集群共享客户端配置。
// 调用方必须已持有 t.mu。
func (t *Transport) buildSaramaConfig(brokers []string, commitInterval time.Duration, batchSize int) *sarama.Config {
	key := strings.Join(brokers, ",")
	if cfg, ok := t.saramaConfigs[key]; ok {
		return cfg
	}
	cfg := sarama.NewConfig()
	if commitInterval > 0 {
		cfg.Consumer.Offsets.CommitInterval = commitInterval
	}
	if batchSize > 0 {
		cfg.Producer.Flush.Messages = batchSize
	}
	t.saramaConfigs[key] = cfg
	return cfg
}

func (t *Transport) publisherFor(brokers []string, batchSize int) (message.Publisher, error) {
	key := strings.Join(brokers, ",")
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.publishers[key]; ok {
		return p, nil
	}
	p, err := t.newPublisher(brokers, t.buildSaramaConfig(brokers, 0, batchSize), t.logger())
	if err != nil {
		return nil, err
	}
	t.publishers[key] = p
	return p, nil
}

func (t *Transport) subscriberFor(brokers []string, group string, commitInterval time.Duration) (message.Subscriber, error) {
	key := strings.Join(brokers, ",") + "|" + group
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.subscribers[key]; ok {
		return s, nil
	}
	s, err := t.newSubscriber(brokers, group, t.buildSaramaConfig(brokers, commitInterval, 0), t.logger())
	if err != nil {
		return nil, err
	}
	t.subscribers[key] = s
	return s, nil
}

// fanIn 合并多个订阅 channel 为单一 channel；全部输入关闭后关闭输出。
func fanIn(chans []<-chan *message.Message) <-chan *message.Message {
	out := make(chan *message.Message)
	var wg sync.WaitGroup
	for _, ch := range chans {
		wg.Add(1)
		go func(ch <-chan *message.Message) {
			defer wg.Done()
			for msg := range ch {
				out <- msg
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

var _ pubsub.Transport = (*Transport)(nil)
var _ lynx.Component = (*Transport)(nil)
```

> 锁纪律：`buildSaramaConfig` **不加锁**（由 `publisherFor`/`subscriberFor` 持 `t.mu` 时调用，避免 `sync.Mutex` 不可重入死锁）。

删除旧文件：`binder.go`、`consumer.go`、`producer.go`、`binder_test.go`、`consumer_test.go`、`producer_test.go`、`fakes_test.go`。

修改 `contrib/kafka/go.mod`（`go mod tidy` 自动完成，无需手改）：

```bash
cd contrib/kafka && go mod tidy
```

预期：`github.com/segmentio/kafka-go`、`github.com/cenkalti/backoff/v5`、`github.com/spf13/cast` 被移除；`github.com/ThreeDotsLabs/watermill-kafka/v3 v3.1.0`、`github.com/IBM/sarama` 加入。若 tidy 失败（版本解析），手工执行：

```bash
go get github.com/ThreeDotsLabs/watermill-kafka/v3@v3.1.0 github.com/IBM/sarama@v1.43.3
```

- [ ] **Step 4: 重写 _examples/pubsub**

创建 `_examples/pubsub/config.yaml`：

```yaml
kafka:
  hello:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_hello]
    consumer:
      group_id: consumer_hello
      instances: 3
      log_message: true
    producer:
      log_message: true
```

用以下内容整体替换 `_examples/pubsub/main.go`：

```go
package main

import (
	"context"
	gohttp "net/http"
	"os"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/kafka"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/lynx-go/lynx/contrib/zap"
	"github.com/lynx-go/lynx/server/http"
	"github.com/lynx-go/x/log"
	"github.com/samber/lo"
)

func main() {
	builder := lynx.NewBuilder(func(ctx context.Context, app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))

		// kafka 配置从 config.yaml 的 kafka 段加载（--config 指定路径）。
		var kafkaOpts kafka.Options
		if err := app.Config().UnmarshalKey("kafka", &kafkaOpts); err != nil {
			return err
		}
		kafkaT, err := kafka.NewTransport(kafkaOpts)
		if err != nil {
			return err
		}
		memT := pubsub.NewMemoryTransport()
		broker := pubsub.NewBroker(pubsub.Options{
			Transports:       []pubsub.Transport{kafkaT},
			DefaultTransport: memT,
		})
		app.Register(memT)
		app.Register(kafkaT)
		app.Register(broker)
		app.Register(pubsub.NewRouter(broker, []pubsub.Handler{&helloHandler{}}))

		mux := gohttp.NewServeMux()
		mux.HandleFunc("/hello", func(writer gohttp.ResponseWriter, request *gohttp.Request) {
			if err := broker.Publish(ctx, "hello",
				pubsub.MustJSONMessage(map[string]any{"message": "hello"}),
				pubsub.WithMessageKey(uuid.NewString()),
			); err != nil {
				log.ErrorContext(ctx, "failed to publish", err)
				writer.WriteHeader(gohttp.StatusInternalServerError)
				return
			}
			_, _ = writer.Write([]byte("ok"))
		})
		hs := http.NewServer(mux, http.WithAddr(":7071"))
		app.Register(hs)

		return nil
	},
		lynx.WithID(lo.Must1(os.Hostname())),
		lynx.WithName("pubsub"),
		lynx.WithUseDefaultConfigFlagsFunc(),
	)
	builder.Run()
}

type helloHandler struct{}

func (h *helloHandler) EventName() string  { return "hello" }
func (h *helloHandler) HandlerName() string { return "helloHandler" }

func (h *helloHandler) HandlerFunc() pubsub.HandlerFunc {
	return func(ctx context.Context, event *pubsub.Message) error {
		log.InfoContext(ctx, "hello event", "payload", string(event.Payload))
		return nil
	}
}

var _ pubsub.Handler = new(helloHandler)
```

> `lynx.WithUseDefaultConfigFlagsFunc()` 使示例支持 `go run main.go --config=config.yaml`（也注册 `--log-level` 等默认 flag）。

- [ ] **Step 5: 运行测试确认通过**

Run: `cd contrib/kafka && go test ./...`
Expected: 全部通过（fake client seam，无需真实 Kafka）。

Run: `cd contrib/pubsub && go test ./...`
Expected: 全部通过（回归）。

Run: `cd _examples && go build ./... && go vet ./...`
Expected: 编译通过（Task 4 造成的过渡态修复）。

- [ ] **Step 6: 提交**

```bash
git add contrib/kafka/ contrib/pubsub/go.mod contrib/pubsub/go.sum _examples/pubsub/
git commit -m "feat(kafka): Transport 组件重写，删除旧 Binder/Consumer/Producer，示例配置化"
```

---

### Task 6: 文档同步与全量回归

**Files:**
- Modify: `README.md`（pubsub/kafka 用法段落）
- Modify: `docs/04-component-system.md`（pubsub 小节）
- Modify: `CLAUDE.md`（PubSub / Kafka Binder 架构描述）
- Modify: `docs/superpowers/specs/2026-08-05-pubsub-redesign-design.md`（如实施与设计有出入，标注偏差）

**Interfaces:**
- Consumes: Task 4/5 的最终公共 API

- [ ] **Step 1: 更新 README.md**

将 README 中 `contrib/pubsub` 示例段（`handler := pubsub.HandlerFunc(func(ctx context.Context, msg *message.Message) error`、`msg := pubsub.NewJSONMessage(...)`）替换为新 API 形态：

```go
import "github.com/lynx-go/lynx/contrib/pubsub"

handler := pubsub.HandlerFunc(func(ctx context.Context, msg *pubsub.Message) error {
    // msg.ID / msg.Key / msg.Headers / msg.Payload
    return nil
})

msg := pubsub.MustJSONMessage(map[string]string{"user": "alice"})
err := broker.Publish(ctx, "user.created", msg, pubsub.WithMessageKey("alice"))
```

将 kafka 示例段（`binder := kafka.NewBinder(kafka.BinderOptions{...})` 及其 SubscribeOptions/PublishOptions 代码块）替换为：

```go
import "github.com/lynx-go/lynx/contrib/kafka"

// 从配置文件加载（config.yaml 的 kafka 段），或代码构造：
kafkaT, err := kafka.NewTransport(kafka.Options{
    Topics: map[string]kafka.TopicOptions{
        "user.created": {
            Brokers: []string{"127.0.0.1:9092"},
            Topics:  []string{"user_created"},
            Consumer: &kafka.ConsumerOptions{GroupID: "users", Instances: 3},
            Producer: &kafka.ProducerOptions{LogMessage: true},
        },
    },
})
broker := pubsub.NewBroker(pubsub.Options{
    Transports:       []pubsub.Transport{kafkaT},
    DefaultTransport: pubsub.NewMemoryTransport(),
})
app.Register(kafkaT, broker)
```

同步检查 README 中其他引用 `Binder`/`NewKafkaMessage` 的位置并修正。

- [ ] **Step 2: 更新 docs/04-component-system.md**

重写"### pubsub：消息发布订阅（Broker/Router/Handler）"小节（约 197-237 行）与 kafka 相关描述：Binder 概念替换为 Transport 概念；代码示例对齐 `_examples/pubsub` 新形态；补充配置示例（`kafka:` 段 + `UnmarshalKey`）。

- [ ] **Step 3: 更新 CLAUDE.md**

将"**PubSub**"与"**Kafka Binder**"两段（CLAUDE.md:159-166）替换为：

```markdown
**PubSub** (contrib/pubsub/)
- Broker 门面组件：topic → Transport 路由表（自动路由 + 显式 Route + 默认回退）
- Transport 接口：后端即组件（kafka/内存），公共 API 使用自有 Message 类型
- Router 组件：Init 期缓冲注册 Handler 订阅，无时序依赖

**Kafka Transport** (contrib/kafka/transport.go)
- 配置驱动：UnmarshalKey("kafka") 加载 map[逻辑topic] 配置（brokers/topics/consumer/producer）
- 内部按 brokers 分组客户端，订阅按（组 × 物理 topic × 实例数）展开后 fan-in
- 基于 watermill-kafka/v3（IBM/sarama）
```

- [ ] **Step 4: 全量回归**

Run（各模块）：

```bash
cd /d/Codes/qiulin/lynx && go vet ./... && go test -race -coverprofile=coverage.out ./...
cd _examples && go vet ./... && go test ./...
cd contrib/kafka && go vet ./... && go test -race ./...
cd contrib/pubsub && go vet ./... && go test -race ./...
cd contrib/metrics && go vet ./... && go test -race ./...
cd contrib/schedule && go vet ./... && go test -race ./...
cd contrib/zap && go vet ./... && go test -race ./...
```

Expected: 全部通过。核心模块覆盖率 ≥ 70%（CI 门槛）：

```bash
go tool cover -func=coverage.out | tail -1
```

若核心覆盖率低于 70%，为 Task 1 的 config 测试补充用例（如 `UnmarshalKey` 错误路径）。

- [ ] **Step 5: 提交**

```bash
git add README.md docs/04-component-system.md CLAUDE.md docs/superpowers/specs/2026-08-05-pubsub-redesign-design.md
git commit -m "docs: 同步 pubsub/kafka 新架构的 README/docs/CLAUDE.md"
```

---

## 完成定义（Definition of Done）

- [ ] 全部 6 个任务完成，所有模块 `go vet` + `go test -race` 通过
- [ ] `contrib/pubsub` 与 `contrib/kafka` 无残留 Binder/Consumer/Producer 旧符号（`grep -rn "Binder\|NewKafkaMessage\|ToConsumerName" contrib/` 无匹配）
- [ ] 核心模块覆盖率 ≥ 70%
- [ ] `_examples/pubsub` 以 `go run main.go --config=config.yaml` 可运行（真实 Kafka 环境）
- [ ] 设计文档与实施无重大偏差
