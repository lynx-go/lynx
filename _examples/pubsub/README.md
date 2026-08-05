# pubsub 示例

基于 Kafka Transport 的发布/订阅示例：HTTP 端点触发发布，consumer group 消费事件。
Kafka 配置驱动：`config.yaml` 的 `kafka` 段定义逻辑 topic（brokers、物理 topics、consumer/producer 参数），`main.go` 通过 `app.Config().UnmarshalKey("kafka", &kafkaOpts)` 加载。

## 依赖

需要本地 Kafka：broker `127.0.0.1:19092`，topic `topic_hello`（地址与 topic 配置在 `config.yaml`）。

## 运行

```bash
go run . --config=config.yaml
# 另开终端触发一次发布
curl http://127.0.0.1:7071/hello
```

## 关键代码点

- `config.yaml` `kafka` 段：逻辑 topic `hello` 的 brokers、物理 topics `[topic_hello]`、consumer（group `consumer_hello`，3 实例）与 producer 配置。
- `main.go` `kafka.NewTransport`：从 `app.Config().UnmarshalKey("kafka", &kafkaOpts)` 加载的 `kafka.Options` 创建 Transport 组件（订阅按消费组 × 物理 topic × 实例数展开）。
- `main.go` `pubsub.NewBroker`：`Transports: [kafkaT]` 自动路由 `hello` 到 Kafka，`DefaultTransport: memT` 兜底未配置 topic。
- `main.go` `pubsub.NewRouter`：将 `helloHandler` 缓冲订阅到 Broker。
- `main.go` `/hello` HTTP 端点（`:7071`）发布 JSON 事件，带 UUID message key。
- `main.go` `helloHandler`：实现 `pubsub.Handler`，消费 `hello` 事件并记录 payload。
