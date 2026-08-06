# pubsub 示例

基于 Kafka Transport 的发布/订阅示例：HTTP 端点触发发布，consumer group 消费事件。
Kafka 配置驱动：`config.yaml` 的 `kafka` 段定义逻辑 topic（brokers、物理 topics、consumer/producer 参数），`main.go` 通过 `app.Config().UnmarshalKey("kafka", &kafkaOpts)` 加载。

## 依赖

需要本地 Kafka：broker `127.0.0.1:19092`，topic `topic_hello`（地址与 topic 配置在 `config.yaml`）。`notify` 路由走内存 transport，不依赖 Kafka。

## 运行

```bash
go run . --config=config.yaml
# 另开终端触发发布（hello 走 Kafka；notify 走内存 transport）
curl http://127.0.0.1:7071/hello
curl http://127.0.0.1:7071/notify
```

## 关键代码点

- `config.yaml` `kafka` 段：逻辑 topic `hello` 的 brokers、物理 topics `[topic_hello]`、consumer（group `consumer_hello`，3 实例）与 producer 配置。
- `config.yaml` `pubsub` 段：显式路由表——逻辑 topic（业务事件名）→ `{transport, key}`，覆盖自动路由。`transport` 为后端标识（`kafka`/`memory`）；`key` 是调用 transport 时的主题名，对 kafka 即 kafka 段配置的逻辑 key（如 `hello→hello`），可与逻辑名不同（如 `notify→user_notify`）。
- `main.go` `kafka.NewTransport`：从 `app.Config().UnmarshalKey("kafka", &kafkaOpts)` 加载的 `kafka.Options` 创建 Transport 组件（订阅按消费组 × 物理 topic × 实例数展开）。
- `main.go` `pubsub.NewBroker`：`Transports: [kafkaT]` 自动路由 `hello` 到 Kafka，`DefaultTransport: memT` 兜底未配置 topic。
- `main.go` 路由装配：从 `pubsub` 段加载路由表，逐条 `broker.RouteKey(topic, transport, key)` 应用；引用了未知 transport 标识时构建期报错。
- `main.go` `pubsub.NewRouter`：将 `helloHandler`、`notifyHandler` 缓冲订阅到 Broker。
- `main.go` `/hello`、`/notify` HTTP 端点（`:7071`）分别发布 JSON 事件，带 UUID message key。
- `main.go` `helloHandler`/`notifyHandler`：实现 `pubsub.Handler`，分别消费 `hello`（Kafka）与 `notify`（内存）事件并记录 payload。
